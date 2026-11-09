package groups_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-platform/account"
	"github.com/getkin/kin-openapi/openapi3"
)

func api(t *testing.T, f *fixture, p account.Principal, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	var e error
	if body != nil {
		data, e = json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
	}
	h := httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return p, nil })
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func shape(t *testing.T, name string, body []byte) {
	t.Helper()
	d, e := openapi3.NewLoader().LoadFromData(g.OpenAPI())
	if e != nil {
		t.Fatal(e)
	}
	var v any
	if e = json.Unmarshal(body, &v); e != nil {
		t.Fatal(e)
	}
	if e = d.Components.Schemas[name].Value.VisitJSON(v); e != nil {
		t.Fatalf("%s: %v\n%s", name, e, body)
	}
}
func problem(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	shape(t, "Problem", w.Body.Bytes())
	var p struct {
		Code string `json:"code"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &p); e != nil {
		t.Fatal(e)
	}
	if p.Code != code {
		t.Fatalf("code=%s want=%s", p.Code, code)
	}
}

func TestEveryCollectionTypedPaginationAndAuthorization(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}
	other := config()
	other.ID = "other"
	other.School = "other-school"
	if _, e := f.s.Configure(ctx, root, "other", other); e != nil {
		t.Fatal(e)
	}
	collections := map[string]string{"members": "MemberPage", "actions": "ActionPage", "reports": "ReportPage", "hits": "HitPage", "messages": "MessagePage", "subscriptions": "SubscriptionPage", "blacklist": "BlacklistPage", "warnings": "WarningsPage", "watch": "WatchPage", "deliveries": "DeliveryPage"}
	e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 1; i <= 52; i++ {
			id := fmt.Sprintf("id%03d", i)
			records := map[string]any{
				"members":       g.Member{Owner: id, Joined: true, State: "restored"},
				"actions":       g.Action{ID: id, Group: "g", Actor: g.Actor(manager), Command: g.Command{Kind: "warn", Target: user.Key(), Text: "reason"}, State: "pending", Created: f.now},
				"reports":       g.Report{Reporter: user.Key(), Message: "m", Reason: "reason", State: "open"},
				"hits":          g.Hit{Owner: user.Key(), Message: "m", Rules: []string{"keyword"}, State: "matched"},
				"messages":      g.Message{ID: id, Owner: user.Key(), Body: "body", Attachments: []string{}, Expires: f.now.Add(time.Hour)},
				"subscriptions": g.Subscription{Receiver: "g", Actor: g.Actor(manager), Enabled: true},
				"blacklist":     g.Blacklist{Reason: "reason", Until: f.now.Add(time.Hour)},
				"warnings":      g.Warnings{Count: 2}, "watch": g.Watch{Enabled: true, Reason: "reason"}, "deliveries": g.Delivery{Receiver: "g", Action: id},
			}
			for collection, record := range records {
				if e := tx.Put("g/"+collection, id, record); e != nil {
					return e
				}
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	for collection, schema := range collections {
		t.Run(collection, func(t *testing.T) {
			path := "/api/v1/groups/g/" + collection
			for _, page := range []struct {
				query  string
				n      int
				cursor string
			}{{"", 50, "id050"}, {"?limit=2", 2, "id002"}, {"?limit=2&cursor=id002", 2, "id004"}, {"?limit=2&cursor=id051", 1, ""}, {"?limit=100", 52, ""}} {
				w := api(t, f, manager, "GET", path+page.query, "", nil)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				shape(t, schema, w.Body.Bytes())
				var p struct {
					Items      []map[string]any `json:"items"`
					NextCursor string           `json:"next_cursor"`
				}
				if e := json.Unmarshal(w.Body.Bytes(), &p); e != nil {
					t.Fatal(e)
				}
				if len(p.Items) != page.n || p.NextCursor != page.cursor {
					t.Fatalf("%s: %+v", page.query, p)
				}
				for i := 1; i < len(p.Items); i++ {
					if p.Items[i-1]["id"].(string) >= p.Items[i]["id"].(string) {
						t.Fatal("unordered page")
					}
				}
			}
			for _, query := range []string{"?limit=0", "?limit=101", "?limit=bad", "?limit=", "?limit=1&limit=2"} {
				problem(t, api(t, f, manager, "GET", path+query, "", nil), 400, "groups_invalid_request")
			}
			problem(t, api(t, f, account.Principal{}, "GET", path, "", nil), 401, "groups_unauthenticated")
			problem(t, api(t, f, outsider, "GET", path, "", nil), 403, "groups_forbidden")
			problem(t, api(t, f, manager, "GET", "/api/v1/groups/other/"+collection, "", nil), 403, "groups_forbidden")
			problem(t, api(t, f, manager, "GET", "/api/v1/groups/missing/"+collection, "", nil), 404, "groups_not_found")
			f.revoked.Store(true)
			problem(t, api(t, f, manager, "GET", path, "", nil), 403, "groups_forbidden")
			f.revoked.Store(false)
		})
	}
	w := api(t, f, manager, "GET", "/api/v1/groups/g/arbitrary", "", nil)
	if w.Code != 404 {
		t.Fatal("generic collection route still exists")
	}
}

func TestGroupSummaryCountsMaskingAndScope(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}
	other := config()
	other.ID = "a-private"
	if _, e := f.s.Configure(ctx, root, "other", other); e != nil {
		t.Fatal(e)
	}
	e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i, state := range []string{"restored", "grace", "extended", "removed"} {
			if e := tx.Put("g/members", fmt.Sprint(i), g.Member{Owner: fmt.Sprint(i), Joined: i < 3, State: state}); e != nil {
				return e
			}
		}
		for i, state := range []string{"pending", "unknown", "succeeded"} {
			if e := tx.Put("g/actions", fmt.Sprint(i), g.Action{State: state}); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	w := api(t, f, manager, "GET", "/api/v1/groups?limit=1", "", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	shape(t, "GroupSummaryPage", w.Body.Bytes())
	var p g.Page[g.GroupSummary]
	if e = json.Unmarshal(w.Body.Bytes(), &p); e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 0 || p.NextCursor != "a-private" {
		t.Fatal(p)
	}
	w = api(t, f, manager, "GET", "/api/v1/groups?limit=1&cursor=a-private", "", nil)
	shape(t, "GroupSummaryPage", w.Body.Bytes())
	if e = json.Unmarshal(w.Body.Bytes(), &p); e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 1 {
		t.Fatal(p)
	}
	v := p.Items[0]
	if v.Name != "测试群" || v.Number != "*****6789" || v.MemberCount != 3 || v.PendingCount != 4 || v.Revision != 1 || !v.BotEnabled || !v.RequireStudent {
		t.Fatalf("summary: %+v", v)
	}
	if strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "123456789") {
		t.Fatal("summary leaked configuration")
	}
}

func TestConfigurationRevisionPasswordAndReplay(t *testing.T) {
	f := setup(t)
	in := config()
	in.ExpectedRevision = 1
	in.Mode = "password"
	in.Password = strptr("private-dynamic-password")
	w := api(t, f, manager, "PUT", "/api/v1/groups/g", "password", in)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	shape(t, "GroupConfig", w.Body.Bytes())
	var updated g.GroupConfig
	if e := json.Unmarshal(w.Body.Bytes(), &updated); e != nil {
		t.Fatal(e)
	}
	if updated.Revision != 2 || !updated.PasswordSet {
		t.Fatal(updated)
	}
	if strings.Contains(w.Body.String(), "password_hash") || strings.Contains(w.Body.String(), "private-dynamic-password") || strings.Contains(w.Body.String(), "password_digest") {
		t.Fatal("secret in response")
	}
	replay := api(t, f, manager, "PUT", "/api/v1/groups/g", "password", in)
	if replay.Code != 200 || replay.Body.String() != w.Body.String() {
		t.Fatal("replay changed result", replay.Body.String())
	}
	in.Name = "stale"
	problem(t, api(t, f, manager, "PUT", "/api/v1/groups/g", "stale", in), 409, "groups_conflict")
	var raw string
	if e := f.pool.QueryRow(ctx, "SELECT string_agg(data::text,' ') FROM groups.records").Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(raw, "private-dynamic-password") {
		t.Fatal("plaintext persisted")
	}
	var record map[string]any
	if e := f.r.Transaction(ctx, func(tx g.Tx) error { return tx.Get("config", "g", &record) }); e != nil {
		t.Fatal(e)
	}
	digest, _ := record["password_digest"].(string)
	if !strings.Contains(digest, ":") {
		t.Fatal("missing salted password digest")
	}
	// Two distinct requests with the same revision cannot both write config/audit/outbox.
	in.Password = nil
	in.Mode = "before"
	in.ExpectedRevision = 2
	beforeAudit, beforeEvents := count(t, f, "g/audit"), 0
	if e := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.outbox").Scan(&beforeEvents); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			copy := in
			copy.Name = fmt.Sprint(i)
			_, e := f.s.Configure(ctx, manager, fmt.Sprint("concurrent", i), copy)
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, g.ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 || count(t, f, "g/audit") != beforeAudit+1 {
		t.Fatal("concurrent revision failure", success, conflict)
	}
	var afterEvents int
	if e := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.outbox").Scan(&afterEvents); e != nil || afterEvents != beforeEvents+1 {
		t.Fatal("conflict emitted event", e)
	}
	current, e := f.s.Configuration(ctx, manager, "g")
	if e != nil || current.Revision != 3 || !current.PasswordSet {
		t.Fatal(current, e)
	}
	in.ExpectedRevision = 3
	in.Password = strptr("")
	in.Number = nil
	current, e = f.s.Configure(ctx, manager, "clear", in)
	if e != nil || current.PasswordSet || current.Number != "*****6789" {
		t.Fatal(current, e)
	}
	data, _ := json.Marshal(in)
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	delete(body, "expected_revision")
	problem(t, api(t, f, manager, "PUT", "/api/v1/groups/g", "missing-rev", body), 422, "groups_unprocessable")
	body["expected_revision"] = 4
	body["password_hash"] = "old"
	problem(t, api(t, f, manager, "PUT", "/api/v1/groups/g", "legacy", body), 422, "groups_unprocessable")
}

func TestCommandDiscriminatorRequiresKindSpecificFields(t *testing.T) {
	f := setup(t)
	cases := map[string][]string{
		"password": {"text"}, "report": {"message", "text"}, "recheck": {"target"}, "extend": {"target", "text", "seconds"}, "exempt": {"target", "text"}, "warn": {"target", "text"}, "mute": {"target", "seconds"}, "unmute": {"target"}, "kick": {"target"}, "blacklist": {"target", "text"}, "unblacklist": {"target"}, "send": {"text"}, "forward": {"message"}, "recall": {"message"}, "card": {"target", "text"}, "analyze": {"target"}, "preview": {"target", "text"}, "rule": {"target", "text", "message"}, "subscribe": {"receiver", "enabled"}, "feedback": {"target", "text"}, "watch": {"target", "enabled"}, "ai_execute": {"target"},
	}
	for kind, required := range cases {
		body := map[string]any{"kind": kind}
		for _, field := range required {
			switch field {
			case "seconds":
				body[field] = 60
			case "enabled":
				body[field] = false
			default:
				body[field] = "value"
			}
		}
		for _, field := range required {
			value := body[field]
			delete(body, field)
			problem(t, api(t, f, manager, "POST", "/api/v1/groups/g/commands", kind+field, body), 422, "groups_unprocessable")
			body[field] = value
		}
	}
	for _, body := range []any{map[string]any{}, map[string]any{"kind": "invented"}, map[string]any{"kind": "mute", "target": "x", "seconds": 0}, map[string]any{"kind": "send", "text": "hello", "target": "x"}, map[string]any{"kind": "dice", "target": user.Key()}, map[string]any{"kind": "subscribe", "receiver": "g", "enabled": nil}} {
		problem(t, api(t, f, manager, "POST", "/api/v1/groups/g/commands", "bad", body), 422, "groups_unprocessable")
	}
	if count(t, f, "g/actions") != 0 {
		t.Fatal("invalid command wrote actions")
	}
	for kind, body := range map[string]any{"mute": map[string]any{"kind": "mute", "target": user.Key(), "seconds": 30}, "send": map[string]any{"kind": "send", "text": "hello"}} {
		w := api(t, f, manager, "POST", "/api/v1/groups/g/commands", kind, body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		shape(t, "Result", w.Body.Bytes())
	}
	if count(t, f, "g/actions") != 2 {
		t.Fatal("valid commands not queued")
	}
	w := api(t, f, manager, "POST", "/api/v1/groups/g/commands", "subscribe", map[string]any{"kind": "subscribe", "receiver": "g", "enabled": false})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestCandidateMigrationToFinalContractAndLeaseRecovery(t *testing.T) {
	f := setup(t)
	e := f.r.Transaction(ctx, func(tx g.Tx) error {
		records := map[string]any{
			"config":               map[string]any{"id": "legacy", "school": "school", "mode": "password", "password_hash": "unsalted", "grace_seconds": 3600, "keywords": []string{}, "auto_actions": []string{}},
			"legacy/actions":       map[string]any{"id": "a", "actor": map[string]string{"Issuer": "test", "Subject": "manager"}, "state": "executing", "result": f.now.Add(-time.Minute).Format(time.RFC3339Nano)},
			"legacy/subscriptions": map[string]any{"actor": map[string]string{"Issuer": "test", "Subject": "manager"}, "receiver": "g", "enabled": true},
			"legacy/blacklist":     map[string]any{"Reason": "reason", "Until": f.now, "Permanent": true},
			"legacy/warnings":      map[string]int{"Count": 3},
		}
		for bucket, value := range records {
			id := "a"
			if bucket == "config" {
				id = "legacy"
			}
			if e := tx.Put(bucket, id, value); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if e = f.r.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	root := account.Principal{Issuer: "test", Subject: "root"}
	c, e := f.s.Configuration(ctx, root, "legacy")
	if e != nil || c.Revision != 1 || c.Name != "legacy" || c.PasswordSet {
		t.Fatalf("%+v %v", c, e)
	}
	for collection, schema := range map[string]string{"actions": "ActionPage", "subscriptions": "SubscriptionPage", "blacklist": "BlacklistPage", "warnings": "WarningsPage"} {
		// Inspect storage keys directly: response structs must not hide a missed migration.
		var stored map[string]any
		if e = f.r.Transaction(ctx, func(tx g.Tx) error { return tx.Get("legacy/"+collection, "a", &stored) }); e != nil {
			t.Fatal(e)
		}
		raw, e := json.Marshal(stored)
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(raw), "Issuer") || strings.Contains(string(raw), "Reason") || strings.Contains(string(raw), "Count") {
			t.Fatalf("unmigrated %s: %s", schema, raw)
		}
	}
	if _, e = f.s.Recover(ctx, "legacy", ""); e != nil {
		t.Fatal(e)
	}
	var action g.Action
	if e = f.r.Transaction(ctx, func(tx g.Tx) error { return tx.Get("legacy/actions", "a", &action) }); e != nil {
		t.Fatal(e)
	}
	if action.State != "unknown" || action.LeaseUntil != nil {
		t.Fatal(action)
	}
	in := config()
	in.ID = "legacy"
	in.Mode = "password"
	in.ExpectedRevision = 1
	in.Password = strptr("replacement")
	updated, e := f.s.Configure(ctx, root, "reset-legacy", in)
	if e != nil || !updated.PasswordSet {
		t.Fatal(updated, e)
	}
	if e = f.r.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	c, e = f.s.Configuration(ctx, root, "legacy")
	if e != nil || !c.PasswordSet || c.Revision != 2 {
		t.Fatal("repeat migration cleared new config", c, e)
	}
	var stored map[string]any
	if e = f.r.Transaction(ctx, func(tx g.Tx) error { return tx.Get("config", "legacy", &stored) }); e != nil {
		t.Fatal(e)
	}
	if _, ok := stored["password_hash"]; ok {
		t.Fatal("legacy password retained")
	}
}

func TestExplicitErrorResponsesAndBulkDiscriminator(t *testing.T) {
	f := setup(t)
	d, e := openapi3.NewLoader().LoadFromData(g.OpenAPI())
	if e != nil {
		t.Fatal(e)
	}
	for path, item := range d.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Responses.Default() != nil {
				t.Fatalf("default response at %s %s", method, path)
			}
			for _, code := range []string{"400", "401", "403", "503"} {
				if path == "/api/v1/groups/me" && code == "403" {
					continue
				}
				if op.Responses.Value(code) == nil {
					t.Fatalf("missing %s at %s %s", code, method, path)
				}
			}
		}
	}
	path := "/api/v1/groups/g/batch"
	invalid := map[string]any{"targets": []string{user.Key()}, "preview": false, "command": map[string]any{"kind": "mute"}}
	problem(t, api(t, f, manager, "POST", path, "bad-batch", invalid), 422, "groups_unprocessable")
	invalid["command"] = map[string]any{"kind": "mute", "seconds": 30, "target": user.Key()}
	problem(t, api(t, f, manager, "POST", path, "extra-target", invalid), 422, "groups_unprocessable")
	invalid["command"] = map[string]any{"kind": "mute", "seconds": 30}
	w := api(t, f, manager, "POST", path, "batch", invalid)
	if w.Code != 200 || count(t, f, "g/actions") != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	shape(t, "BulkResult", w.Body.Bytes())
	problem(t, api(t, f, manager, "POST", "/api/v1/groups/g/commands", "", map[string]any{"kind": "send", "text": "hello"}), 400, "groups_invalid_request")
	c := config()
	c.BotEnabled = false
	if _, e = configureCurrent(t, f, manager, "disable-bot", c); e != nil {
		t.Fatal(e)
	}
	problem(t, api(t, f, manager, "POST", "/api/v1/groups/g/commands", "disabled", map[string]any{"kind": "send", "text": "hello"}), 422, "groups_disabled")
	h := httpapi.Handler(&g.Service{}, func(*http.Request) (account.Principal, error) { return manager, nil })
	r := httptest.NewRequest("GET", "/api/v1/groups", nil)
	unavailable := httptest.NewRecorder()
	h.ServeHTTP(unavailable, r)
	problem(t, unavailable, 503, "groups_unavailable")
}
