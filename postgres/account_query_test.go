package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	aq "github.com/StuHelper/stuhelper-groups/accountquery"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"os"
	"testing"
	"time"
)

func TestAccountQueryScopePaginationAndRuntime(t *testing.T) {
	dsn := os.Getenv("STUHELPER_GROUPS_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	ctx := context.Background()
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	admin, e := sql.Open("pgx", cfg.ConnString())
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	name := fmt.Sprintf("iq_%d", time.Now().UnixNano())
	if _, e = admin.Exec(`CREATE DATABASE ` + name); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
	cfg.Database = name
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	if _, e = db.Exec(Schema); e != nil {
		t.Fatal(e)
	}
	exec := func(s string, args ...any) {
		t.Helper()
		if _, e := db.Exec(s, args...); e != nil {
			t.Fatal(e)
		}
	}
	target := aq.Principal{Issuer: "issuer", Subject: "target"}
	for _, v := range []struct{ id, school string }{{"a", "other"}, {"b", "school"}, {"c", "school"}} {
		exec(`INSERT INTO groups.records(bucket,id,data) VALUES('config',$1,jsonb_build_object('school',$2::text))`, v.id, v.school)
		exec(`INSERT INTO groups.records(bucket,id,data) VALUES($1,$2,jsonb_build_object('owner',$2::text,'state','eligible','joined',true))`, v.id+"/members", target.Key())
	}
	exec(`INSERT INTO groups.records(bucket,id,data) VALUES('b/actions','action',jsonb_build_object('command',jsonb_build_object('target',$1::text,'kind','mute'),'state','done'))`, target.Key())
	exec("INSERT INTO groups.records(bucket,id,data) VALUES('*/blacklist',$1,'{\"permanent\":true}'),('b/blacklist',$1,'{\"permanent\":false,\"until\":\"2000-01-01T00:00:00Z\"}'),('b/watch',$1,'{\"enabled\":false}')", target.Key())
	exec("INSERT INTO groups.records(bucket,id,data) VALUES('*/audit','blocked',jsonb_build_object('target',$1::text,'action','blacklist','result','blocked'))", target.Key())
	exec("INSERT INTO groups.records(bucket,id,data) SELECT 'b/messages',i::text,'{}' FROM generate_series(1,10000) i")
	exec("ANALYZE groups.records")
	var plan string
	scopes := `[{"school":"*","group":"*"}]`
	if err := db.QueryRow("EXPLAIN (FORMAT JSON) "+accountQuerySQL, target.Key(), "", scopes, aq.Domain, 100, "[]", target.Issuer, target.Subject).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	var nodes []struct{ Plan map[string]any }
	if err := json.Unmarshal([]byte(plan), &nodes); err != nil {
		t.Fatal(err)
	}
	var inspect func(map[string]any)
	inspect = func(node map[string]any) {
		if node["Node Type"] == "Seq Scan" && node["Alias"] == "r" {
			t.Error("scanned unrelated messages")
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			inspect(child.(map[string]any))
		}
	}
	inspect(nodes[0].Plan)
	// Repeating final-schema initialization must preserve stored records.
	if _, e = db.Exec(Schema); e != nil {
		t.Fatal(e)
	}
	role := name + "_reader"
	if _, e = admin.Exec(`CREATE ROLE ` + role + ` LOGIN`); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(`DROP ROLE ` + role)
	defer admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`)
	exec(`GRANT USAGE ON SCHEMA groups TO ` + role)
	exec(`GRANT SELECT ON ALL TABLES IN SCHEMA groups TO ` + role)
	cfg.User = role
	runtimeDB := stdlib.OpenDB(*cfg)
	defer runtimeDB.Close()
	if _, e = runtimeDB.Exec(`DELETE FROM groups.records`); e == nil {
		t.Fatal("reader modified source")
	}
	poolCfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	poolCfg.ConnConfig = cfg
	pool, e := pgxpool.NewWithConfig(ctx, poolCfg)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	repo := &Repository{Pool: pool}
	scope := aq.Scope{School: "school", Group: "*", Module: aq.Domain}

	s := aq.Selection{Query: aq.Query{Kind: "account", Value: target.Key(), Limit: 1}, Scopes: []aq.Scope{scope}, SensitiveScopes: []aq.Scope{}}
	ids := map[string]bool{}
	for {
		page, e := repo.ReadAccount(ctx, s)
		if e != nil {
			t.Fatal(e)
		}
		if len(page) == 0 {
			break
		}
		if len(page) != 1 || ids[page[0].ID] || !scope.Contains(page[0].Scope) {
			t.Fatal("scope or page", page)
		}
		ids[page[0].ID] = true
		s.Query.After = page[0].ID
	}
	if len(ids) != 5 {
		t.Fatal("unexpected filtered records", ids)
	}
	s.Query.After = ""
	s.Query.Limit = 100
	s.Scopes = []aq.Scope{{School: "*", Group: "*", Module: aq.Domain}}
	all, err := repo.ReadAccount(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	global := 0
	inactive := 0
	disabled := false
	for _, r := range all {
		if r.Scope.Group == "*" {
			global++
			if r.Scope.School != "*" {
				t.Fatal(r)
			}
		}
		if !r.Current {
			inactive++
		}
		for _, f := range r.Fields {
			if f.Name == "enabled" && f.Value == "false" {
				disabled = true
			}
		}
	}
	if global != 2 || inactive != 2 || !disabled {
		t.Fatalf("global=%d inactive=%d disabled=%t", global, inactive, disabled)
	}

	s.Scopes = []aq.Scope{{School: "school", Group: "ungranted", Module: aq.Domain}}
	page, e := repo.ReadAccount(ctx, s)
	if e != nil || len(page) != 0 {
		t.Fatal("group scope bypass", page, e)
	}

}
