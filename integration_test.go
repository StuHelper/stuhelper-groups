package groups_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-groups/postgres"
	"github.com/StuHelper/stuhelper-platform/account"
	"github.com/StuHelper/stuhelper-platform/admin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ctx = context.Background()
var manager = account.Principal{Issuer: "test", Subject: "manager"}
var user = account.Principal{Issuer: "test", Subject: "user"}
var outsider = account.Principal{Issuer: "test", Subject: "outsider"}

type identity struct{}

func (identity) PlatformAdmin(_ context.Context, p account.Principal) (bool, error) {
	return p.Subject == "root", nil
}

type authStore struct {
	admin.Store
	revoked *atomic.Bool
}

func (a authStore) Effective(_ context.Context, owner string) (admin.Effective, error) {
	e := admin.Effective{}
	if owner == manager.Key() && !a.revoked.Load() {
		e.Grants = []admin.Grant{{Role: admin.Role{Capabilities: []string{"groups.read", "groups.manage", "groups.configure", "groups.messages.read", "groups.messages.send", "groups.messages.recall"}}, Scope: admin.Scope{School: "school", Group: "g", Module: "groups"}}}
	}
	return e, nil
}

type authRepo struct{ store admin.Store }

func (a authRepo) View(c context.Context, f func(admin.Store) error) error  { return f(a.store) }
func (a authRepo) Write(c context.Context, f func(admin.Store) error) error { return f(a.store) }

type facts struct {
	mu    sync.Mutex
	value g.Facts
	err   error
}

func (f *facts) Current(context.Context, string, string) (g.Facts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.value, f.err
}
func (f *facts) set(v bool) { f.mu.Lock(); defer f.mu.Unlock(); f.value.Student = v }

type bot struct {
	mu      sync.Mutex
	calls   int
	out     map[string]g.Outcome
	timeout bool
	failure bool
}

func (b *bot) Execute(_ context.Context, a g.Action) (g.Outcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.out[a.ID]; ok {
		return v, nil
	}
	b.calls++
	v := g.Outcome{Success: !b.failure, Code: "actual_result"}
	b.out[a.ID] = v
	if b.timeout {
		return g.Outcome{}, context.DeadlineExceeded
	}
	return v, nil
}
func (b *bot) Lookup(_ context.Context, id string) (g.Outcome, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.out[id]
	return v, ok, nil
}

type fixture struct {
	s       *g.Service
	r       *postgres.Repository
	pool    *pgxpool.Pool
	facts   *facts
	bot     *bot
	now     time.Time
	revoked atomic.Bool
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("STUHELPER_GROUPS_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL via scripts/check-with-postgres.sh")
	}
	adminPool, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	id := "groups_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, e = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{id}.Sanitize())
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.Database = id
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		_, e := adminPool.Exec(ctx, "DROP DATABASE "+pgx.Identifier{id}.Sanitize()+" WITH (FORCE)")
		if e != nil {
			t.Error(e)
		}
		adminPool.Close()
	})
	f := &fixture{r: &postgres.Repository{Pool: pool}, pool: pool, now: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), facts: &facts{value: g.Facts{Bound: true, Student: true, StudentKnown: true, StageKnown: true, Fields: map[string]string{"name": "测试", "gender": "unknown", "nickname": "自由昵称"}}}, bot: &bot{out: map[string]g.Outcome{}}}
	if e = f.r.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = f.r.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	f.s = &g.Service{Repo: f.r, PlatformIdentity: identity{}, Auth: admin.Service{Repo: authRepo{authStore{revoked: &f.revoked}}, Identity: identity{}}, Facts: f.facts, Bot: f.bot, Now: func() time.Time { return f.now }, Random: func(n int64) (int64, error) { return 0, nil }}
	_, e = f.s.Configure(ctx, manager, "setup", config())
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func config() g.Config {
	return g.Config{ID: "g", School: "school", Mode: "before", RequireStudent: true, GraceSeconds: 3600, ReminderSeconds: 600, RetentionSeconds: 120, DiceSides: 6, MuteProbability: 100, MuteSeconds: 60, CooldownSeconds: 30, Pity: 3, CheckCard: true, CardTemplate: "{name}{separator}{nickname}", Keywords: []string{"spam"}}
}
func command(t *testing.T, f *fixture, p account.Principal, key string, c g.Command) g.Result {
	t.Helper()
	v, e := f.s.Command(ctx, p, "g", key, c)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func count(t *testing.T, f *fixture, b string) int {
	t.Helper()
	var n int
	if e := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.records WHERE bucket=$1", b).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func actions(t *testing.T, f *fixture) []g.Action {
	t.Helper()
	p, e := f.s.List(ctx, manager, "g", "actions", "", 100)
	if e != nil {
		t.Fatal(e)
	}
	out := []g.Action{}
	for _, b := range p.Items {
		var a g.Action
		if e = json.Unmarshal(b, &a); e != nil {
			t.Fatal(e)
		}
		out = append(out, a)
	}
	return out
}
func TestQQAdmissionGraceRestorationExtensionExemptionAndRemoval(t *testing.T) {
	f := setup(t)
	f.facts.set(false)
	r := command(t, f, user, "apply", g.Command{Kind: "apply"})
	if r.State != "awaiting_verification" {
		t.Fatal(r)
	}
	f.facts.set(true)
	r = command(t, f, manager, "qualified", g.Command{Kind: "recheck", Target: user.Key()})
	if r.State != "restored" {
		t.Fatal(r)
	}
	f.facts.set(false)
	command(t, f, manager, "lost", g.Command{Kind: "recheck", Target: user.Key()})
	m, e := f.s.Progress(ctx, user, "g")
	if e != nil || m.State != "grace" || m.Deadline != f.now.Add(time.Hour) {
		t.Fatalf("%+v %v", m, e)
	}
	for _, a := range actions(t, f) {
		if a.Command.Kind == "kick" {
			t.Fatal("early kick")
		}
	}
	command(t, f, manager, "extend", g.Command{Kind: "extend", Target: user.Key(), Text: "approved", Seconds: 7200})
	f.now = f.now.Add(time.Hour)
	command(t, f, manager, "still", g.Command{Kind: "recheck", Target: user.Key()})
	m, _ = f.s.Progress(ctx, user, "g")
	if m.State != "extended" {
		t.Fatal(m)
	}
	f.now = f.now.Add(2 * time.Hour)
	r = command(t, f, manager, "expired", g.Command{Kind: "recheck", Target: user.Key()})
	if r.State != "removal_pending" {
		t.Fatal(r)
	}
	f.facts.set(true)
	a, e := f.s.Execute(ctx, "g", r.ID)
	if e != nil || a.State != "cancelled" || f.bot.calls != 0 {
		t.Fatalf("%+v %v", a, e)
	}
	command(t, f, manager, "restored", g.Command{Kind: "recheck", Target: user.Key()})
	f.facts.set(false)
	command(t, f, manager, "lost2", g.Command{Kind: "recheck", Target: user.Key()})
	command(t, f, manager, "exempt", g.Command{Kind: "exempt", Target: user.Key(), Text: "approved"})
	f.now = f.now.Add(48 * time.Hour)
	r = command(t, f, manager, "exemptCheck", g.Command{Kind: "recheck", Target: user.Key()})
	if r.State != "exempt" {
		t.Fatal(r)
	}
	other := account.Principal{Issuer: "test", Subject: "other"}
	f.facts.set(true)
	command(t, f, other, "otherApply", g.Command{Kind: "apply"})
	f.facts.set(false)
	command(t, f, manager, "otherLost", g.Command{Kind: "recheck", Target: other.Key()})
	f.now = f.now.Add(time.Hour)
	r = command(t, f, manager, "otherExpired", g.Command{Kind: "recheck", Target: other.Key()})
	a, e = f.s.Execute(ctx, "g", r.ID)
	if e != nil || a.State != "succeeded" {
		t.Fatalf("%+v %v", a, e)
	}
	m, _ = f.s.Progress(ctx, other, "g")
	if m.State != "removed" {
		t.Fatal(m)
	}
}
func TestConcurrentIdempotencyAndActualBotOutcomes(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.s.Command(ctx, manager, "g", "same", g.Command{Kind: "warn", Target: user.Key(), Text: "reason"})
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if count(t, f, "g/actions") != 1 {
		t.Fatal("duplicate actions")
	}
	_, e := f.s.Command(ctx, manager, "g", "same", g.Command{Kind: "kick", Target: user.Key()})
	if !errors.Is(e, g.ErrConflict) {
		t.Fatal(e)
	}
	a := actions(t, f)[0]
	f.bot.timeout = true
	out, e := f.s.Execute(ctx, "g", a.ID)
	if e == nil || out.State != "unknown" {
		t.Fatalf("%+v %v", out, e)
	}
	out, e = f.s.Execute(ctx, "g", a.ID)
	if e != nil || out.State != "succeeded" || f.bot.calls != 1 {
		t.Fatalf("%+v %v calls=%d", out, e, f.bot.calls)
	}
	r := command(t, f, manager, "deniedByQQ", g.Command{Kind: "kick", Target: user.Key()})
	f.bot.timeout = false
	f.bot.failure = true
	out, e = f.s.Execute(ctx, "g", r.ID)
	if e != nil || out.State != "failed" {
		t.Fatalf("%+v %v", out, e)
	}
}
func TestScopedAuthorizationRevocationAndSensitiveReads(t *testing.T) {
	f := setup(t)
	_, e := f.s.Configuration(ctx, outsider, "g")
	if !errors.Is(e, g.ErrForbidden) {
		t.Fatal(e)
	}
	c := config()
	c.ID = "other"
	_, e = f.s.Configure(ctx, manager, "outside", c)
	if !errors.Is(e, g.ErrForbidden) {
		t.Fatal(e)
	}
	_, e = f.s.Command(ctx, user, "g", "spoof", g.Command{Kind: "self_mute", Target: outsider.Key()})
	if !errors.Is(e, g.ErrForbidden) {
		t.Fatal(e)
	}
	r := command(t, f, manager, "pending", g.Command{Kind: "kick", Target: user.Key()})
	f.revoked.Store(true)
	_, e = f.s.Execute(ctx, "g", r.ID)
	if !errors.Is(e, g.ErrForbidden) || f.bot.calls != 0 {
		t.Fatal(e)
	}
	_, e = f.s.List(ctx, manager, "g", "messages", "", 100)
	if !errors.Is(e, g.ErrForbidden) {
		t.Fatal(e)
	}
}
func TestBEEntertainmentDisabledSelfOnlyCooldownAndNoViolation(t *testing.T) {
	f := setup(t)
	command(t, f, user, "apply", g.Command{Kind: "apply"})
	if e := f.s.MemberEvent(ctx, "g", event("joined", "qq-bot", "g", "groups.member_joined", 1), user.Key()); e != nil {
		t.Fatal(e)
	}
	r := command(t, f, user, "dice", g.Command{Kind: "dice"})
	if r.Value != 1 {
		t.Fatal(r)
	}
	r = command(t, f, user, "mute", g.Command{Kind: "self_mute"})
	if r.State != "pending" {
		t.Fatal(r)
	}
	a, e := f.s.Execute(ctx, "g", r.ID)
	if e != nil || a.Command.Target != user.Key() || a.Command.Text != "entertainment" {
		t.Fatalf("%+v %v", a, e)
	}
	if count(t, f, "g/hits") != 0 {
		t.Fatal("entertainment recorded as violation")
	}
	_, e = f.s.Command(ctx, user, "g", "again", g.Command{Kind: "self_mute"})
	if !errors.Is(e, g.ErrCooldown) {
		t.Fatal(e)
	}
	_, e = f.s.Command(ctx, user, "g", "unmute", g.Command{Kind: "unmute", Target: user.Key()})
	if !errors.Is(e, g.ErrForbidden) {
		t.Fatal(e)
	}
	c := config()
	c.DiceSides = 0
	c.MuteProbability = 0
	_, e = f.s.Configure(ctx, manager, "disable", c)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.s.Command(ctx, user, "g", "disabled", g.Command{Kind: "dice"})
	if !errors.Is(e, g.ErrDisabled) {
		t.Fatal(e)
	}
}
func event(id, source, subject, kind string, revision int64) g.Event {
	return g.Event{SpecVersion: "1.0", ID: id, Source: source, Subject: subject, Type: kind, Time: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), Revision: revision, Data: json.RawMessage(`{}`)}
}
func TestRetentionInboxRollbackAndNoRecallViewAudit(t *testing.T) {
	f := setup(t)
	e := event("m1", "qq-bot", "g", "groups.message", 2)
	m := g.Message{ID: "m1", Owner: user.Key(), Body: "private recalled body", Recalled: true}
	if err := f.s.Ingest(ctx, "g", e, m); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Ingest(ctx, "g", e, m); err != nil {
		t.Fatal(err)
	}
	old := event("old", "qq-bot", "g", "groups.message", 1)
	m.ID = "old"
	if err := f.s.Ingest(ctx, "g", old, m); err != nil {
		t.Fatal(err)
	}
	if count(t, f, "g/messages") != 1 {
		t.Fatal("duplicate or stale stored")
	}
	before := count(t, f, "g/audit")
	page, err := f.s.List(ctx, manager, "g", "messages", "", 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	if count(t, f, "g/audit") != before {
		t.Fatal("recall view audit")
	}
	f.now = f.now.Add(120 * time.Second)
	page, err = f.s.List(ctx, manager, "g", "messages", "", 10)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("expired content exposed")
	}
	if _, err = f.s.Cleanup(ctx, "g", ""); err != nil {
		t.Fatal(err)
	}
	if count(t, f, "g/messages") != 0 {
		t.Fatal("content retained")
	}
	e.Data = json.RawMessage(`{"changed":true}`)
	if err = f.s.Ingest(ctx, "g", e, m); !errors.Is(err, g.ErrConflict) {
		t.Fatal(err)
	}
	command(t, f, user, "apply", g.Command{Kind: "apply"})
	f.facts.err = fmt.Errorf("unavailable")
	change := event("change", "verification", user.Key(), "verification.changed", 1)
	if err = f.s.Consume(ctx, "g", change, user.Key()); !errors.Is(err, g.ErrUnavailable) {
		t.Fatal(err)
	}
	var n int
	if err = f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.inbox WHERE id='change'").Scan(&n); err != nil || n != 0 {
		t.Fatal("failed consumer committed inbox", err)
	}
	f.facts.err = nil
	if err = f.s.Consume(ctx, "g", change, user.Key()); err != nil {
		t.Fatal(err)
	}
}
func TestRulesCardsReportAndBatchPreview(t *testing.T) {
	f := setup(t)
	r := command(t, f, manager, "card", g.Command{Kind: "card", Target: user.Key(), Text: "wrong"})
	if r.State != "noncompliant" || r.Detail != "测试·自由昵称" {
		t.Fatal(r)
	}
	if count(t, f, "g/actions") != 0 {
		t.Fatal("card caused punishment")
	}
	r = command(t, f, manager, "preview", g.Command{Kind: "preview", Target: user.Key(), Text: "spam"})
	if r.State != "suggested" || count(t, f, "g/hits") != 0 {
		t.Fatal(r)
	}
	r = command(t, f, manager, "rule", g.Command{Kind: "rule", Target: user.Key(), Text: "spam", Message: "m"})
	if count(t, f, "g/hits") != 1 || count(t, f, "g/actions") != 0 {
		t.Fatal(r)
	}
	r = command(t, f, user, "report", g.Command{Kind: "report", Text: "reason", Message: "m"})
	if count(t, f, "g/reports") != 1 {
		t.Fatal(r)
	}
	b, e := f.s.Batch(ctx, manager, "g", "batch", g.Bulk{Targets: []string{user.Key(), outsider.Key()}, Command: g.Command{Kind: "kick"}, Preview: true})
	if e != nil || len(b.Items) != 2 || count(t, f, "g/actions") != 0 {
		t.Fatalf("%+v %v", b, e)
	}
}
func TestHTTPRejectsUnknownFieldsAndNeverTrustsRequestActor(t *testing.T) {
	f := setup(t)
	h := httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return user, nil })
	for _, body := range []string{`{"kind":"kick","target":"victim","admin":true}`, `{"kind":"dice"} {}`, `{"kind":"self_mute","target":"victim"}`} {
		r := httptest.NewRequest("POST", "/api/v1/groups/g/commands", strings.NewReader(body))
		r.Header.Set("Idempotency-Key", "http")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 && w.Code != 403 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	h = httpapi.Handler(f.s, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/groups/g", nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestOutboxOrderingBackoffHeldRetryAndNotificationRollback(t *testing.T) {
	f := setup(t)
	conn, e := f.pool.Acquire(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Release()
	if _, e = conn.Exec(ctx, "LISTEN groups_outbox"); e != nil {
		t.Fatal(e)
	}
	sentinel := errors.New("abort")
	e = f.r.Transaction(ctx, func(tx g.Tx) error {
		if e := tx.Emit(event("rollback", "groups", "rollback", "test", 1)); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	_, e = conn.Conn().WaitForNotification(wait)
	cancel()
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("rollback notified: %v", e)
	}
	for i := 0; i < 3; i++ {
		command(t, f, manager, fmt.Sprintf("warn%d", i), g.Command{Kind: "warn", Target: user.Key(), Text: "reason"})
	}
	events, e := f.r.Pending(ctx, f.now)
	if e != nil || len(events) != 1 {
		t.Fatalf("%d %v", len(events), e)
	}
	first := events[0]
	if e = f.r.Failed(ctx, first.ID, f.now); e != nil {
		t.Fatal(e)
	}
	events, e = f.r.Pending(ctx, f.now.Add(4*time.Second))
	if e != nil || len(events) != 0 {
		t.Fatal("stream overtook retry", e)
	}
	events, e = f.r.Pending(ctx, f.now.Add(5*time.Second))
	if e != nil || len(events) != 1 || events[0].ID != first.ID {
		t.Fatal("retry deadline", e)
	}
	if e = f.r.Failed(ctx, first.ID, f.now.Add(25*time.Hour)); e != nil {
		t.Fatal(e)
	}
	events, e = f.r.Pending(ctx, f.now.Add(26*time.Hour))
	if e != nil || len(events) != 0 {
		t.Fatal("held stream bypassed", e)
	}
	if e = f.r.Retry(ctx, first.ID, f.now.Add(26*time.Hour)); e != nil {
		t.Fatal(e)
	}
	events, e = f.r.Pending(ctx, f.now.Add(26*time.Hour))
	if e != nil || len(events) != 1 || events[0].ID != first.ID {
		t.Fatal("retry changed event", e)
	}
	if e = f.r.Delivered(ctx, first.ID, f.now); e != nil {
		t.Fatal(e)
	}
	events, e = f.r.Pending(ctx, f.now)
	if e != nil || len(events) != 1 || events[0].Revision <= first.Revision {
		t.Fatal("outbox ordering", e)
	}
}
func TestRuntimeDatabaseRoleIsolationAndMigration(t *testing.T) {
	f := setup(t)
	role := "groups_runtime_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	q := pgx.Identifier{role}.Sanitize()
	if _, e := f.pool.Exec(ctx, "CREATE ROLE "+q+" LOGIN"); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(ctx, "DROP OWNED BY "+q); _, _ = f.pool.Exec(ctx, "DROP ROLE "+q) })
	if _, e := f.pool.Exec(ctx, "GRANT USAGE ON SCHEMA groups TO "+q+"; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA groups TO "+q+"; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA groups TO "+q+"; CREATE SCHEMA private_other; CREATE TABLE private_other.secret(id int)"); e != nil {
		t.Fatal(e)
	}
	cfg := f.pool.Config().Copy()
	cfg.ConnConfig.User = role
	runtime, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer runtime.Close()
	r := &postgres.Repository{Pool: runtime}
	if e = r.Transaction(ctx, func(tx g.Tx) error { return tx.Put("runtime", "test", map[string]bool{"ok": true}) }); e != nil {
		t.Fatal(e)
	}
	for _, sql := range []string{"SELECT * FROM private_other.secret", "CREATE TABLE groups.forbidden(id int)", "DROP SCHEMA groups CASCADE"} {
		if _, e = runtime.Exec(ctx, sql); e == nil {
			t.Fatalf("runtime exceeded ownership: %s", sql)
		}
	}
}

type ai struct{}

func (ai) Analyze(context.Context, string) (string, []string, error) {
	return "建议警告", []string{"warn", "kick", "blacklist"}, nil
}
func TestAISuggestionsRequireEnabledActionsAndCurrentAuthorization(t *testing.T) {
	f := setup(t)
	f.s.AI = ai{}
	e := event("message", "qq-bot", "g", "groups.message", 1)
	if err := f.s.Ingest(ctx, "g", e, g.Message{ID: "m", Owner: user.Key(), Body: "message"}); err != nil {
		t.Fatal(err)
	}
	r := command(t, f, user, "report", g.Command{Kind: "report", Message: "m", Text: "reason"})
	analysis := command(t, f, manager, "analyze", g.Command{Kind: "analyze", Target: r.ID})
	if analysis.Detail == "" || count(t, f, "g/actions") != 0 {
		t.Fatal(analysis)
	}
	command(t, f, manager, "noRules", g.Command{Kind: "ai_execute", Target: r.ID})
	if count(t, f, "g/actions") != 0 || count(t, f, "g/blacklist") != 0 {
		t.Fatal("unconfigured AI action")
	}
	c := config()
	c.AutoActions = []string{"warn"}
	if _, err := f.s.Configure(ctx, manager, "enableWarn", c); err != nil {
		t.Fatal(err)
	}
	command(t, f, manager, "allowedAI", g.Command{Kind: "ai_execute", Target: r.ID})
	a := actions(t, f)
	if len(a) != 1 || a[0].Command.Kind != "warn" {
		t.Fatal(a)
	}
}
func TestPasswordDoesNotGrantStudentEligibilityAndBlacklistScope(t *testing.T) {
	f := setup(t)
	c := config()
	c.Mode = "password"
	c.PasswordHash = "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b"
	if _, e := f.s.Configure(ctx, manager, "passwordConfig", c); e != nil {
		t.Fatal(e)
	}
	f.facts.set(false)
	command(t, f, user, "apply", g.Command{Kind: "apply"})
	r := command(t, f, user, "pass", g.Command{Kind: "password", Text: "secret"})
	if r.State == "restored" {
		t.Fatal("password granted eligibility")
	}
	f.facts.set(true)
	r = command(t, f, manager, "qualified", g.Command{Kind: "recheck", Target: user.Key()})
	if r.State != "restored" {
		t.Fatal(r)
	}
	_, e := f.s.Blacklists(ctx, manager, "global", g.BlacklistInput{Groups: []string{"*"}, Target: user.Key(), Reason: "reason"})
	if !errors.Is(e, g.ErrForbidden) {
		t.Fatal(e)
	}
	root := account.Principal{Issuer: "test", Subject: "root"}
	if _, e = f.s.Blacklists(ctx, root, "global", g.BlacklistInput{Groups: []string{"*"}, Target: user.Key(), Reason: "reason"}); e != nil {
		t.Fatal(e)
	}
	r = command(t, f, manager, "blocked", g.Command{Kind: "recheck", Target: user.Key()})
	if r.State == "restored" {
		t.Fatal(r)
	}
}
func TestRelayDrainsAtStartupAndWakesOnCommit(t *testing.T) {
	f := setup(t)
	f.r.Now = func() time.Time { return f.now }
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan g.Event, 10)
	done := make(chan error, 1)
	go func() {
		done <- f.r.Relay(relayCtx, func(_ context.Context, e g.Event) error { events <- e; return nil })
	}()
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("startup failed")
	}
	r := command(t, f, manager, "wake", g.Command{Kind: "warn", Target: user.Key(), Text: "reason"})
	select {
	case e := <-events:
		if !strings.Contains(string(e.Data), r.ID) {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("commit notification failed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not stop")
	}
}
func TestSubscriptionTargetsRemainIndependentAndBatchPayloadConflict(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}
	for _, id := range []string{"receiver1", "receiver2"} {
		c := config()
		c.ID = id
		if _, e := f.s.Configure(ctx, root, id, c); e != nil {
			t.Fatal(e)
		}
		command(t, f, root, "subscribe"+id, g.Command{Kind: "subscribe", Receiver: id, Enabled: true})
	}
	r := command(t, f, manager, "warn", g.Command{Kind: "warn", Target: user.Key(), Text: "reason"})
	if _, e := f.s.Execute(ctx, "g", r.ID); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e := f.pool.QueryRow(ctx, "SELECT envelope FROM groups.outbox WHERE envelope->>'type'='groups.action.result' ORDER BY sequence DESC LIMIT 1").Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var ev g.Event
	if e := json.Unmarshal(raw, &ev); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if e := f.s.Fanout(ctx, ev); e != nil {
			t.Fatal(e)
		}
	}
	for i, id := range []string{"receiver1", "receiver2"} {
		page, e := f.s.List(ctx, root, id, "actions", "", 100)
		if e != nil || len(page.Items) != 1 {
			t.Fatalf("%s %+v %v", id, page, e)
		}
		var a g.Action
		if e = json.Unmarshal(page.Items[0], &a); e != nil {
			t.Fatal(e)
		}
		f.bot.failure = i == 1
		a, e = f.s.Execute(ctx, id, a.ID)
		if e != nil {
			t.Fatal(e)
		}
		want := "succeeded"
		if i == 1 {
			want = "failed"
		}
		if a.State != want {
			t.Fatal(a)
		}
	}
	input := g.Bulk{Targets: []string{user.Key()}, Command: g.Command{Kind: "kick"}}
	if _, e := f.s.Batch(ctx, manager, "g", "batch", input); e != nil {
		t.Fatal(e)
	}
	input.Targets = []string{outsider.Key()}
	if _, e := f.s.Batch(ctx, manager, "g", "batch", input); !errors.Is(e, g.ErrConflict) {
		t.Fatal(e)
	}
}
func TestApplicationCannotClaimMembershipForEntertainment(t *testing.T) {
	f := setup(t)
	command(t, f, user, "apply", g.Command{Kind: "apply"})
	if _, e := f.s.Command(ctx, user, "g", "dice", g.Command{Kind: "dice"}); !errors.Is(e, g.ErrForbidden) {
		t.Fatal(e)
	}
	joined := event("join", "qq-bot", "g", "groups.member_joined", 1)
	if e := f.s.MemberEvent(ctx, "g", joined, user.Key()); e != nil {
		t.Fatal(e)
	}
	if e := f.s.MemberEvent(ctx, "g", joined, user.Key()); e != nil {
		t.Fatal(e)
	}
	command(t, f, user, "joinedDice", g.Command{Kind: "dice"})
	left := event("left", "qq-bot", "g", "groups.member_left", 2)
	if e := f.s.MemberEvent(ctx, "g", left, user.Key()); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Command(ctx, user, "g", "leftDice", g.Command{Kind: "dice"}); !errors.Is(e, g.ErrForbidden) {
		t.Fatal(e)
	}
}
func TestConcurrentEventDeduplicationAndExecutionClaim(t *testing.T) {
	f := setup(t)
	ev := event("concurrent", "qq-bot", "g", "groups.message", 1)
	message := g.Message{ID: "concurrent", Owner: user.Key(), Body: "content"}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.s.Ingest(ctx, "g", ev, message) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if count(t, f, "g/messages") != 1 {
		t.Fatal("duplicated messages")
	}
	var n int
	if e := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.outbox WHERE envelope->>'type'='groups.message.changed'").Scan(&n); e != nil || n != 1 {
		t.Fatal("duplicated event", e)
	}
	message.Body = "changed"
	if e := f.s.Ingest(ctx, "g", ev, message); !errors.Is(e, g.ErrConflict) {
		t.Fatal(e)
	}
	r := command(t, f, manager, "concurrentAction", g.Command{Kind: "warn", Target: user.Key(), Text: "reason"})
	errs = make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := f.s.Execute(ctx, "g", r.ID); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil && !errors.Is(e, g.ErrConflict) {
			t.Fatal(e)
		}
	}
	if f.bot.calls != 1 {
		t.Fatal("duplicate bot calls", f.bot.calls)
	}
}
