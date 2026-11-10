package postgres

import (
	"context"
	"database/sql"
	"fmt"
	m "github.com/StuHelper/stuhelper-groups"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"os"
	"testing"
	"time"
)

func queryWriteDatabase(t *testing.T) (*sql.DB, *pgx.ConnConfig) {
	t.Helper()
	dsn := os.Getenv("STUHELPER_GROUPS_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	name := fmt.Sprintf("query_write_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE DATABASE ` + name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg.Database = name
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { db.Close(); _, _ = admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`); admin.Close() })
	if _, err = db.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	var triggers int
	if err = db.QueryRow(`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='groups' AND p.proname='account_query_changed'`).Scan(&triggers); err != nil || triggers != 0 {
		t.Fatal("unexpected timestamp trigger function", triggers, err)
	}
	return db, cfg
}

func TestAccountQueryRepositoryWritesRefreshTime(t *testing.T) {
	db, cfg := queryWriteDatabase(t)
	ctx := context.Background()
	pc, err := pgxpool.ParseConfig(os.Getenv("STUHELPER_GROUPS_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig = cfg
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repo := &Repository{Pool: pool}
	for _, bucket := range []string{"config", "g/members", "g/actions", "g/blacklist", "g/warnings", "g/watch", "g/audit"} {
		t.Run(bucket, func(t *testing.T) {
			err := repo.Transaction(ctx, func(port m.Tx) error {
				tx := port.(*transaction)
				if e := port.Put(bucket, "record", map[string]string{"state": "pending"}); e != nil {
					return e
				}
				var id string
				var previous time.Time
				if e := tx.tx.QueryRow(ctx, `SELECT query_id::text,query_changed_at FROM groups.records WHERE bucket=$1 AND id='record'`, bucket).Scan(&id, &previous); e != nil {
					return e
				}
				if id == "" || previous.IsZero() {
					t.Fatal("missing query metadata")
				}
				for _, state := range []string{"executing", "done"} {
					if e := port.Put(bucket, "record", map[string]string{"state": state}); e != nil {
						return e
					}
					var nextID, savedState string
					var at time.Time
					if e := tx.tx.QueryRow(ctx, `SELECT query_id::text,query_changed_at,data->>'state' FROM groups.records WHERE bucket=$1 AND id='record'`, bucket).Scan(&nextID, &at, &savedState); e != nil {
						return e
					}
					if nextID != id || !at.After(previous) || savedState != state {
						t.Fatalf("Put(%s): ID %s -> %s, time %s -> %s, state %s", bucket, id, nextID, previous, at, savedState)
					}
					previous = at
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	snapshot := func() string {
		t.Helper()
		var out string
		if e := db.QueryRow(`SELECT jsonb_agg(to_jsonb(r) ORDER BY bucket,id)::text FROM groups.records r`).Scan(&out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	before := snapshot()
	if err = repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot() != before {
		t.Fatal("initialization changed IDs, timestamps or payloads")
	}
	rollback := fmt.Errorf("rollback")
	err = repo.Transaction(ctx, func(tx m.Tx) error {
		if e := tx.Put("g/members", "record", map[string]string{"state": "removed"}); e != nil {
			return e
		}
		return rollback
	})
	if err != rollback || snapshot() != before {
		t.Fatal("rollback changed query metadata or payload", err)
	}
}
