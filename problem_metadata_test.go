package groups_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-groups/sdk"
	"github.com/StuHelper/stuhelper-platform/account"
)

type problemDetail struct {
	Code              string `json:"code"`
	Reason            string `json:"reason"`
	Field             string `json:"field"`
	RetryAfterSeconds *int64 `json:"retry_after_seconds"`
}

func decodeProblem(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) problemDetail {
	t.Helper()
	problem(t, w, wantStatus, wantCode)
	var out problemDetail
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	return out
}

func TestProblemSubcodesRetryAfterAndConfigFieldDiagnostics(t *testing.T) {
	f := setup(t)
	command(t, f, user, "u-apply", g.Command{Kind: "apply"})
	if e := f.s.MemberEvent(ctx, "g", event("u-join", "qq-bot", "g", "groups.member_joined", 1), user.Key()); e != nil {
		t.Fatal(e)
	}

	// 1. Trigger self_mute (MuteSeconds=60, CooldownSeconds=30), advance 12.4s -> 48s remaining (ceil(47.6s)=48).
	w := api(t, f, user, "POST", "/api/v1/groups/g/commands", "sm-1", map[string]any{"kind": "self_mute"})
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	f.now = f.now.Add(12400 * time.Millisecond)
	cd := api(t, f, user, "POST", "/api/v1/groups/g/commands", "sm-2", map[string]any{"kind": "self_mute"})
	pd := decodeProblem(t, cd, 409, "groups_cooldown")
	if pd.Reason != "member_cooldown" || pd.RetryAfterSeconds == nil || *pd.RetryAfterSeconds != 48 || cd.Header().Get("Retry-After") != "48" {
		t.Fatalf("unexpected member cooldown problem: %+v header=%q", pd, cd.Header().Get("Retry-After"))
	}

	// 2. Action queue full (1000 pending actions) returns action_queue_full without member Retry-After.
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 0; i < 1000; i++ {
			id := fmt.Sprintf("q-%04d", i)
			if e := tx.Put("g/actions", id, g.Action{ID: id, Group: "g", State: "pending"}); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	qFull := api(t, f, manager, "POST", "/api/v1/groups/g/commands", "warn-full", map[string]any{"kind": "warn", "target": user.Key(), "text": "full"})
	pq := decodeProblem(t, qFull, 409, "groups_cooldown")
	if pq.Reason != "action_queue_full" || pq.RetryAfterSeconds != nil || qFull.Header().Get("Retry-After") != "" {
		t.Fatalf("unexpected queue full problem: %+v header=%q", pq, qFull.Header().Get("Retry-After"))
	}
	// Clear synthetic actions for subsequent checks.
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 0; i < 1000; i++ {
			if e := tx.Delete("g/actions", fmt.Sprintf("q-%04d", i)); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}

	// 3. Config cross-field linkage diagnostics and conflict subcodes.
	badAutoMute := config()
	badAutoMute.ExpectedRevision = 1
	badAutoMute.MuteProbability = 0
	badAutoMute.MuteSeconds = 0
	badAutoMute.AutoActions = []string{"mute"}
	pm := decodeProblem(t, api(t, f, manager, "PUT", "/api/v1/groups/g", "cfg-no-mute-sec", badAutoMute), 422, "groups_unprocessable")
	if pm.Reason != "missing_mute_seconds" || pm.Field != "mute_seconds" {
		t.Fatalf("expected missing_mute_seconds on mute_seconds, got %+v", pm)
	}

	badCooldown := config()
	badCooldown.ExpectedRevision = 1
	badCooldown.CooldownSeconds = 0
	pc := decodeProblem(t, api(t, f, manager, "PUT", "/api/v1/groups/g", "cfg-no-cd", badCooldown), 422, "groups_unprocessable")
	if pc.Reason != "missing_cooldown_seconds" || pc.Field != "cooldown_seconds" {
		t.Fatalf("expected missing_cooldown_seconds, got %+v", pc)
	}

	badPassword := config()
	badPassword.ExpectedRevision = 1
	badPassword.Mode = "password"
	badPassword.Password = nil
	pp := decodeProblem(t, api(t, f, manager, "PUT", "/api/v1/groups/g", "cfg-no-pwd", badPassword), 422, "groups_unprocessable")
	if pp.Reason != "missing_password" || pp.Field != "password" {
		t.Fatalf("expected missing_password, got %+v", pp)
	}

	badDice := config()
	badDice.ExpectedRevision = 1
	badDice.DiceSides = 1
	pDice := decodeProblem(t, api(t, f, manager, "PUT", "/api/v1/groups/g", "cfg-dice-1", badDice), 422, "groups_unprocessable")
	if pDice.Reason != "invalid_field" || pDice.Field != "dice_sides" {
		t.Fatalf("expected invalid_field on dice_sides, got %+v", pDice)
	}

	staleCfg := config()
	staleCfg.ExpectedRevision = 99
	pRev := decodeProblem(t, api(t, f, manager, "PUT", "/api/v1/groups/g", "cfg-stale-rev", staleCfg), 409, "groups_conflict")
	if pRev.Reason != "revision_mismatch" || pRev.Field != "expected_revision" {
		t.Fatalf("expected revision_mismatch, got %+v", pRev)
	}

	root := account.Principal{Issuer: "test", Subject: "root"}
	wrongSchool := config()
	wrongSchool.ExpectedRevision = 1
	wrongSchool.School = "other-school"
	pSch := decodeProblem(t, api(t, f, root, "PUT", "/api/v1/groups/g", "cfg-school-change", wrongSchool), 409, "groups_conflict")
	if pSch.Reason != "school_immutable" || pSch.Field != "school" {
		t.Fatalf("expected school_immutable, got %+v", pSch)
	}

	// Idempotency mismatch and disabled subcodes.
	_ = api(t, f, manager, "POST", "/api/v1/groups/g/commands", "idem-1", map[string]any{"kind": "send", "text": "one"})
	pIdem := decodeProblem(t, api(t, f, manager, "POST", "/api/v1/groups/g/commands", "idem-1", map[string]any{"kind": "send", "text": "two"}), 409, "groups_conflict")
	if pIdem.Reason != "idempotency_mismatch" {
		t.Fatalf("expected idempotency_mismatch, got %+v", pIdem)
	}

	pPwdOff := decodeProblem(t, api(t, f, user, "POST", "/api/v1/groups/g/commands", "pwd-off", map[string]any{"kind": "password", "text": "123"}), 422, "groups_disabled")
	if pPwdOff.Reason != "password_mode_disabled" || pPwdOff.Field != "mode" {
		t.Fatalf("expected password_mode_disabled, got %+v", pPwdOff)
	}
}

func TestEntertainmentStructuredResultMetadataAndSDK(t *testing.T) {
	f := setup(t)
	// Configure 20-sided dice, 25% mute probability, 90s mute, 10s cooldown, pity=3.
	c := config()
	c.DiceSides = 20
	c.MuteProbability = 25
	c.MuteSeconds = 90
	c.CooldownSeconds = 10
	c.Pity = 3
	if _, e := configureCurrent(t, f, manager, "ent-cfg", c); e != nil {
		t.Fatal(e)
	}
	// Roll 80 on [0,100) so probability 25 misses until pity triggers on attempt 3; roll 13 on 20-sided dice -> 14.
	f.s.Random = func(n int64) (int64, error) {
		if n == 20 {
			return 13, nil
		}
		return 80, nil
	}

	command(t, f, user, "u-apply", g.Command{Kind: "apply"})
	if e := f.s.MemberEvent(ctx, "g", event("u-join", "qq-bot", "g", "groups.member_joined", 1), user.Key()); e != nil {
		t.Fatal(e)
	}

	server := httptest.NewServer(httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return user, nil }))
	defer server.Close()
	client, e := sdk.NewClientWithResponses(server.URL)
	if e != nil {
		t.Fatal(e)
	}

	var diceCmd sdk.Command
	_ = diceCmd.FromDiceCommand(sdk.DiceCommand{Kind: "dice"})
	diceRes, e := client.CommandWithResponse(ctx, "g", &sdk.CommandParams{IdempotencyKey: "dice-sdk"}, diceCmd)
	if e != nil || diceRes.StatusCode() != http.StatusOK || diceRes.JSON200 == nil {
		t.Fatalf("dice SDK failed: %+v %v", diceRes, e)
	}
	if diceRes.JSON200.Value != 14 || diceRes.JSON200.MaxValue == nil || *diceRes.JSON200.MaxValue != 20 || diceRes.JSON200.Detail != "dice_sides" {
		t.Fatalf("unexpected dice metadata: %+v", diceRes.JSON200)
	}

	// Attempts 1 and 2 miss with pity_remaining 2 -> 1; attempt 3 hits pity with seconds=90 and pity_remaining=0.
	for i, wantPity := range []int64{2, 1} {
		if i > 0 {
			f.now = f.now.Add(15 * time.Second)
		}
		r := command(t, f, user, fmt.Sprintf("miss-%d", i), g.Command{Kind: "self_mute"})
		if r.State != "completed" || r.Detail != "self_mute_missed" || r.Seconds == nil || *r.Seconds != 0 || r.PityRemaining == nil || *r.PityRemaining != wantPity {
			t.Fatalf("attempt %d unexpected miss metadata: %+v", i+1, r)
		}
	}
	f.now = f.now.Add(15 * time.Second)
	hit := command(t, f, user, "pity-hit", g.Command{Kind: "self_mute"})
	if hit.State != "pending" || hit.Detail != "self_mute_triggered" || hit.Seconds == nil || *hit.Seconds != 90 || hit.PityRemaining == nil || *hit.PityRemaining != 0 {
		t.Fatalf("unexpected pity hit metadata: %+v", hit)
	}
}
