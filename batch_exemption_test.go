package groups_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
)

func TestUnexemptLifecycleAndExtendAntiShrinkProtection(t *testing.T) {
	f := setup(t)
	ta := account.Principal{Issuer: "test", Subject: "ta"}
	guest := account.Principal{Issuer: "test", Subject: "guest"}
	applicant := account.Principal{Issuer: "test", Subject: "applicant"}

	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		if e := tx.Put("g/members", ta.Key(), g.Member{Owner: ta.Key(), Joined: true, Exempt: true, State: "exempt"}); e != nil {
			return e
		}
		if e := tx.Put("g/members", guest.Key(), g.Member{Owner: guest.Key(), Joined: true, Exempt: true, State: "exempt"}); e != nil {
			return e
		}
		return tx.Put("g/members", applicant.Key(), g.Member{Owner: applicant.Key(), Joined: false, Exempt: true, State: "exempt"})
	}); e != nil {
		t.Fatal(e)
	}

	// Eligible TA transitions immediately to restored upon unexempt.
	f.facts.set(true)
	res := command(t, f, manager, "unexempt-ta", g.Command{Kind: "unexempt", Target: ta.Key(), Text: "term_ended"})
	if res.State != "restored" || res.Action != "unexempt" {
		t.Fatalf("expected restored after unexempt, got %+v", res)
	}
	m, e := f.s.Progress(ctx, ta, "g")
	if e != nil || m.Exempt || m.State != "restored" || !m.Deadline.IsZero() {
		t.Fatalf("unexpected TA state: %+v %v", m, e)
	}

	// Ineligible joined guest enters a fresh grace period and receives reminder.
	f.facts.set(false)
	beforeAudit := count(t, f, "g/audit")
	res = command(t, f, manager, "unexempt-guest", g.Command{Kind: "unexempt", Target: guest.Key(), Text: "exchange_ended"})
	if res.State != "grace" {
		t.Fatalf("expected grace after unexempt, got %+v", res)
	}
	m, e = f.s.Progress(ctx, guest, "g")
	if e != nil || m.Exempt || m.State != "grace" || !m.Deadline.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("unexpected guest state: %+v %v", m, e)
	}
	if count(t, f, "g/audit") <= beforeAudit {
		t.Fatal("unexempt did not write audit")
	}

	// Unexempt also works when BotEnabled is false (transitions state without failing on bot queue).
	c := config()
	c.BotEnabled = false
	if _, e := configureCurrent(t, f, manager, "pause-bot", c); e != nil {
		t.Fatal(e)
	}
	res = command(t, f, manager, "unexempt-applicant", g.Command{Kind: "unexempt", Target: applicant.Key(), Text: "recheck_applicant"})
	if res.State != "awaiting_verification" {
		t.Fatalf("expected awaiting_verification for unjoined before-mode applicant, got %+v", res)
	}
	c.BotEnabled = true
	if _, e := configureCurrent(t, f, manager, "resume-bot", c); e != nil {
		t.Fatal(e)
	}

	// Extend never shrinks an existing longer deadline; expired deadlines extend from now.
	sevenDays := f.now.Add(7 * 24 * time.Hour)
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		return tx.Put("g/members", guest.Key(), g.Member{Owner: guest.Key(), Joined: true, State: "grace", Deadline: sevenDays})
	}); e != nil {
		t.Fatal(e)
	}
	command(t, f, manager, "extend-plus-1d", g.Command{Kind: "extend", Target: guest.Key(), Text: "extra_day", Seconds: 86400})
	m, _ = f.s.Progress(ctx, guest, "g")
	wantEightDays := sevenDays.Add(24 * time.Hour)
	if !m.Deadline.Equal(wantEightDays) || !m.Extended || m.State != "extended" {
		t.Fatalf("extend shrank deadline: got %v, want %v", m.Deadline, wantEightDays)
	}

	// Once deadline is in the past, extend adds seconds to s.now().
	f.now = wantEightDays.Add(2 * time.Hour)
	command(t, f, manager, "extend-from-now", g.Command{Kind: "extend", Target: guest.Key(), Text: "second_chance", Seconds: 1800})
	m, _ = f.s.Progress(ctx, guest, "g")
	if !m.Deadline.Equal(f.now.Add(1800 * time.Second)) {
		t.Fatalf("expired deadline did not extend from now: got %v, want %v", m.Deadline, f.now.Add(1800*time.Second))
	}

	// Authorization, discriminator and removed-member checks.
	problem(t, api(t, f, outsider, "POST", "/api/v1/groups/g/commands", "forbid-unexempt", map[string]any{"kind": "unexempt", "target": ta.Key(), "text": "no"}), 403, "groups_forbidden")
	problem(t, api(t, f, manager, "POST", "/api/v1/groups/g/commands", "missing-reason", map[string]any{"kind": "unexempt", "target": ta.Key()}), 422, "groups_unprocessable")
}

func TestBatchDryRunPreviewAccuracyAuthorizationAndLongIdempotencyKey(t *testing.T) {
	f := setup(t)
	joinedGrace := account.Principal{Issuer: "test", Subject: "joined-grace"}
	exemptMember := account.Principal{Issuer: "test", Subject: "exempt-member"}
	unjoinedApplicant := account.Principal{Issuer: "test", Subject: "applicant"}
	missingTarget := account.Principal{Issuer: "test", Subject: "missing"}

	graceDeadline := f.now.Add(2 * time.Hour)
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		if e := tx.Put("g/members", joinedGrace.Key(), g.Member{Owner: joinedGrace.Key(), Joined: true, State: "grace", Deadline: graceDeadline}); e != nil {
			return e
		}
		if e := tx.Put("g/members", exemptMember.Key(), g.Member{Owner: exemptMember.Key(), Joined: true, Exempt: true, State: "exempt"}); e != nil {
			return e
		}
		return tx.Put("g/members", unjoinedApplicant.Key(), g.Member{Owner: unjoinedApplicant.Key(), Joined: false, State: "awaiting_verification"})
	}); e != nil {
		t.Fatal(e)
	}

	// Preview enforces group authorization (403) and group existence (404), never returning HTTP 200 on unauthorized calls.
	longKey := strings.Repeat("k", 128)
	problem(t, api(t, f, outsider, "POST", "/api/v1/groups/g/batch", longKey, map[string]any{
		"targets": []string{joinedGrace.Key()},
		"preview": true,
		"command": map[string]any{"kind": "kick"},
	}), 403, "groups_forbidden")
	problem(t, api(t, f, manager, "POST", "/api/v1/groups/nonexistent/batch", longKey, map[string]any{
		"targets": []string{joinedGrace.Key()},
		"preview": true,
		"command": map[string]any{"kind": "kick"},
	}), 404, "groups_not_found")

	before := databaseDigest(t, f)

	// 1. Kick preview distinguishes joined member, exempt member, unjoined applicant, and missing target.
	w := api(t, f, manager, "POST", "/api/v1/groups/g/batch", longKey, map[string]any{
		"targets": []string{joinedGrace.Key(), exemptMember.Key(), unjoinedApplicant.Key(), missingTarget.Key()},
		"preview": true,
		"command": map[string]any{"kind": "kick"},
	})
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	shape(t, "BulkResult", w.Body.Bytes())
	var kickPreview g.BulkResult
	if e := json.Unmarshal(w.Body.Bytes(), &kickPreview); e != nil {
		t.Fatal(e)
	}
	if kickPreview.Items[0].Result.State != "would_queue" || kickPreview.Items[0].Result.Detail != "grace->pending" {
		t.Fatalf("unexpected grace kick preview: %+v", kickPreview.Items[0])
	}
	if kickPreview.Items[1].Result.State != "would_queue" || kickPreview.Items[1].Result.Detail != "exempt->pending" {
		t.Fatalf("unexpected exempt kick preview: %+v", kickPreview.Items[1])
	}
	if kickPreview.Items[2].Result.State != "not_joined" || kickPreview.Items[2].Result.Detail != "awaiting_verification" {
		t.Fatalf("unexpected applicant kick preview: %+v", kickPreview.Items[2])
	}
	if kickPreview.Items[3].Result.State != "not_found" {
		t.Fatalf("unexpected missing kick preview: %+v", kickPreview.Items[3])
	}

	// 2. Extend preview shows exact anti-shrink target deadline.
	extendPreview, e := f.s.Batch(ctx, manager, "g", longKey, g.Bulk{
		Targets: []string{joinedGrace.Key(), missingTarget.Key()},
		Preview: true,
		Command: g.Command{Kind: "extend", Text: "delay", Seconds: 3600},
	})
	if e != nil {
		t.Fatal(e)
	}
	wantDetail := "grace->extended@" + graceDeadline.Add(time.Hour).Format(time.RFC3339)
	if extendPreview.Items[0].Result.State != "would_apply" || extendPreview.Items[0].Result.Detail != wantDetail || extendPreview.Items[1].Result.State != "not_found" {
		t.Fatalf("unexpected extend preview: %+v", extendPreview)
	}

	// 3. Unexempt preview predicts post-unexempt state from current facts.
	f.facts.set(false)
	unexemptPreview, e := f.s.Batch(ctx, manager, "g", longKey, g.Bulk{
		Targets: []string{exemptMember.Key(), joinedGrace.Key()},
		Preview: true,
		Command: g.Command{Kind: "unexempt", Text: "audit_cleanup"},
	})
	if e != nil {
		t.Fatal(e)
	}
	if unexemptPreview.Items[0].Result.State != "would_apply" || unexemptPreview.Items[0].Result.Detail != "exempt->grace" || unexemptPreview.Items[1].Result.State != "noop" {
		t.Fatalf("unexpected unexempt preview: %+v", unexemptPreview)
	}

	// 4. Card preview distinguishes compliant vs noncompliant without queuing bot actions.
	compliantPreview, e := f.s.Batch(ctx, manager, "g", longKey, g.Bulk{
		Targets: []string{joinedGrace.Key()},
		Preview: true,
		Command: g.Command{Kind: "card", Text: "测试·自由昵称"},
	})
	if e != nil || compliantPreview.Items[0].Result.State != "compliant" {
		t.Fatalf("expected compliant card preview: %+v %v", compliantPreview, e)
	}

	// Dry-run preview must leave the entire database untouched.
	if before != databaseDigest(t, f) {
		t.Fatal("batch preview mutated database")
	}

	// Real batch execution supports unexempt and 128-char Idempotency-Key.
	execRes := api(t, f, manager, "POST", "/api/v1/groups/g/batch", longKey, map[string]any{
		"targets": []string{exemptMember.Key()},
		"preview": false,
		"command": map[string]any{"kind": "unexempt", "text": "rotate_ta"},
	})
	if execRes.Code != 200 {
		t.Fatalf("batch unexempt failed: %d %s", execRes.Code, execRes.Body.String())
	}
	shape(t, "BulkResult", execRes.Body.Bytes())
	m, err := f.s.Progress(ctx, exemptMember, "g")
	if err != nil || m.Exempt || m.State != "grace" {
		t.Fatalf("batch unexempt did not persist: %+v %v", m, err)
	}
	if _, err := f.s.Batch(ctx, manager, "g", longKey, g.Bulk{Targets: []string{joinedGrace.Key()}, Command: g.Command{Kind: "kick"}}); !errors.Is(err, g.ErrConflict) {
		t.Fatalf("expected conflict when reusing 128-char batch key with different input, got %v", err)
	}
}
