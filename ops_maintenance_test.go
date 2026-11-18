package groups_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/StuHelper/stuhelper-platform/admin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-groups/sdk"
	"github.com/StuHelper/stuhelper-platform/account"
)

func TestCancelAndRetryActionsQueueRecoveryAndGroupAudit(t *testing.T) {
	f := setup(t)

	// 1. Fill pending action queue to capacity (1000), confirm groups_cooldown, then rescue via cancel_action!
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 0; i < 1000; i++ {
			id := fmt.Sprintf("stuck-%04d", i)
			if e := tx.Put("g/actions", id, g.Action{
				ID: id, Group: "g", Actor: g.Actor(manager), Capability: "groups.manage",
				Command: g.Command{Kind: "warn", Target: user.Key(), Text: "queued"},
				State:   "pending", Created: f.now,
			}); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Command(ctx, manager, "g", "blocked-by-cap", g.Command{Kind: "warn", Target: user.Key(), Text: "blocked"}); !errors.Is(e, g.ErrCooldown) {
		t.Fatalf("expected ErrCooldown at 1000 pending actions, got %v", e)
	}

	cancelRes := command(t, f, manager, "rescue-cancel", g.Command{Kind: "cancel_action", Target: "stuck-0000", Text: "clear_queue"})
	if cancelRes.State != "cancelled" || cancelRes.ID != "stuck-0000" {
		t.Fatalf("unexpected cancel_action result: %+v", cancelRes)
	}
	// Queue capacity is now 999 -> new action succeeds!
	newWarn := command(t, f, manager, "after-rescue", g.Command{Kind: "warn", Target: user.Key(), Text: "recovered"})
	if newWarn.State != "pending" {
		t.Fatalf("queue remained blocked after cancelling pending action: %+v", newWarn)
	}
	// Clean synthetic stuck actions.
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 1; i < 1000; i++ {
			if e := tx.Delete("g/actions", fmt.Sprintf("stuck-%04d", i)); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}

	// 2. cancel_action rejects executing/unknown/failed/succeeded actions; Execute treats cancelled action as no-op.
	if a, e := f.s.Execute(ctx, "g", "stuck-0000"); e != nil || a.State != "cancelled" || f.bot.calls != 0 {
		t.Fatalf("cancelled action executed: %+v %v calls=%d", a, e, f.bot.calls)
	}
	for _, nonPending := range []string{"executing", "unknown", "failed", "succeeded"} {
		id := "act-" + nonPending
		if e := f.r.Transaction(ctx, func(tx g.Tx) error {
			return tx.Put("g/actions", id, g.Action{ID: id, Group: "g", State: nonPending, Capability: "groups.manage", Command: g.Command{Kind: "kick", Target: user.Key()}})
		}); e != nil {
			t.Fatal(e)
		}
		if _, e := f.s.Command(ctx, manager, "g", "cancel-"+nonPending, g.Command{Kind: "cancel_action", Target: id}); !errors.Is(e, g.ErrConflict) {
			t.Fatalf("cancel_action accepted %s state: %v", nonPending, e)
		}
	}

	// 3. retry_action only retries confirmed failed actions once, enqueues a fresh Action ID, and executes on Bot.
	for _, nonFailed := range []string{"pending", "executing", "unknown", "succeeded", "cancelled"} {
		id := "retry-guard-" + nonFailed
		if e := f.r.Transaction(ctx, func(tx g.Tx) error {
			return tx.Put("g/actions", id, g.Action{ID: id, Group: "g", State: nonFailed, Capability: "groups.manage", Command: g.Command{Kind: "kick", Target: user.Key()}})
		}); e != nil {
			t.Fatal(e)
		}
		if _, e := f.s.Command(ctx, manager, "g", "retry-"+nonFailed, g.Command{Kind: "retry_action", Target: id}); !errors.Is(e, g.ErrConflict) {
			t.Fatalf("retry_action accepted %s state: %v", nonFailed, e)
		}
	}

	f.bot.failure = true
	orig := command(t, f, manager, "kick-fail", g.Command{Kind: "kick", Target: user.Key()})
	failedAct, e := f.s.Execute(ctx, "g", orig.ID)
	if e != nil || failedAct.State != "failed" {
		t.Fatalf("expected failed action, got %+v %v", failedAct, e)
	}
	f.bot.failure = false

	retryRes := command(t, f, manager, "kick-retry", g.Command{Kind: "retry_action", Target: orig.ID, Text: "adapter_recovered"})
	if retryRes.State != "pending" || retryRes.ID == orig.ID || retryRes.Detail != orig.ID {
		t.Fatalf("unexpected retry_action result: %+v", retryRes)
	}
	// Attempting to retry the same failed action a second time with a new key is rejected.
	if _, e := f.s.Command(ctx, manager, "g", "kick-retry-dup", g.Command{Kind: "retry_action", Target: orig.ID}); !errors.Is(e, g.ErrConflict) {
		t.Fatalf("expected ErrConflict on duplicate retry of already retried action, got %v", e)
	}
	retriedAct, e := f.s.Execute(ctx, "g", retryRes.ID)
	if e != nil || retriedAct.State != "succeeded" {
		t.Fatalf("retried action did not succeed: %+v %v", retriedAct, e)
	}

	// 4. GET /api/v1/groups/{group}/audit requires groups.configure, paginates, and never contains recalled message bodies.
	if e := f.s.Ingest(ctx, "g", event("m-secret", "qq-bot", "g", "groups.message", 1), g.Message{ID: "m-secret", Owner: user.Key(), Body: "NEVER_IN_AUDIT_BODY", Recalled: true}); e != nil {
		t.Fatal(e)
	}
	_, _ = f.s.Messages(ctx, manager, "g", "", 10)
	command(t, f, manager, "recall-cmd", g.Command{Kind: "recall", Message: "m-secret"})

	problem(t, api(t, f, outsider, "GET", "/api/v1/groups/g/audit", "", nil), 403, "groups_forbidden")
	auditW := api(t, f, manager, "GET", "/api/v1/groups/g/audit?limit=100", "", nil)
	if auditW.Code != 200 {
		t.Fatalf("audit GET failed: %d %s", auditW.Code, auditW.Body.String())
	}
	shape(t, "AuditPage", auditW.Body.Bytes())
	if strings.Contains(auditW.Body.String(), "NEVER_IN_AUDIT_BODY") {
		t.Fatal("audit endpoint exposed recalled message body")
	}
	var auditPage g.Page[g.AuditEntry]
	_ = json.Unmarshal(auditW.Body.Bytes(), &auditPage)
	hasCancel, hasRetry := false, false
	for _, entry := range auditPage.Items {
		if entry.Action == "cancel_action" && entry.Target == "stuck-0000" {
			hasCancel = true
		}
		if entry.Action == "retry_action" && entry.Target == orig.ID && entry.Result == retryRes.ID {
			hasRetry = true
		}
	}
	if !hasCancel || !hasRetry {
		t.Fatalf("missing cancel/retry audit entries: %+v", auditPage.Items)
	}

	// Verify generated Go SDK client for ListAuditWithResponse.
	server := httptest.NewServer(httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return manager, nil }))
	defer server.Close()
	client, e := sdk.NewClientWithResponses(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	sdkAudit, e := client.ListAuditWithResponse(ctx, "g", nil)
	if e != nil || sdkAudit.StatusCode() != http.StatusOK || sdkAudit.JSON200 == nil || len(sdkAudit.JSON200.Items) == 0 {
		t.Fatalf("sdk ListAudit failed: %+v %v", sdkAudit, e)
	}
}

func TestPeriodicMaintenanceCleansExpiredReceiptsSentOutboxAndFrequencyWindows(t *testing.T) {
	f := setup(t)
	c := config()
	c.FrequencySeconds = 60
	c.FrequencyLimit = 5
	if _, e := configureCurrent(t, f, manager, "freq-cfg", c); e != nil {
		t.Fatal(e)
	}

	// Create old receipt, batch_input, confirmed outbox event, unsent outbox event, and frequency window at t0.
	command(t, f, manager, "old-cmd", g.Command{Kind: "rule", Target: user.Key(), Message: "m1", Text: "hello"})
	if _, e := f.s.Batch(ctx, manager, "g", "old-batch", g.Bulk{Targets: []string{user.Key()}, Command: g.Command{Kind: "warn", Text: "old"}}); e != nil {
		t.Fatal(e)
	}
	f.r.Now = func() time.Time { return f.now }
	pendingEvents, e := f.r.PendingOutbox(ctx, 100, nil)
	if e != nil || len(pendingEvents) < 2 {
		t.Fatalf("expected outbox events, got %d %v", len(pendingEvents), e)
	}
	// Confirm only the first event; leave remaining events unsent so we prove unsent events are never deleted!
	if e := f.r.ConfirmOutbox(ctx, pendingEvents[0].ID); e != nil {
		t.Fatal(e)
	}

	// Advance 25 hours, create a fresh receipt & fresh frequency window for outsider.
	f.now = f.now.Add(25 * time.Hour)
	command(t, f, manager, "fresh-cmd", g.Command{Kind: "rule", Target: outsider.Key(), Message: "m2", Text: "fresh"})

	beforeReceipts := count(t, f, "receipts")
	if beforeReceipts < 3 || count(t, f, "batch_inputs") != 1 || count(t, f, "g/frequency") != 2 {
		t.Fatalf("unexpected pre-cleanup counts: receipts=%d batch=%d freq=%d", beforeReceipts, count(t, f, "batch_inputs"), count(t, f, "g/frequency"))
	}

	if next, e := f.s.CleanupIdempotency(ctx, "receipts", "", 24*time.Hour); e != nil || next != "" {
		t.Fatalf("CleanupIdempotency(receipts): next=%q err=%v", next, e)
	}
	if next, e := f.s.CleanupIdempotency(ctx, "batch_inputs", "", 24*time.Hour); e != nil || next != "" {
		t.Fatalf("CleanupIdempotency(batch_inputs): next=%q err=%v", next, e)
	}
	// Only the 25h-old receipts & batch_inputs were deleted; the fresh receipt remains!
	if count(t, f, "receipts") != 1 || count(t, f, "batch_inputs") != 0 {
		t.Fatalf("unexpected post-cleanup idempotency counts: receipts=%d batch=%d", count(t, f, "receipts"), count(t, f, "batch_inputs"))
	}

	deletedOutbox, e := f.s.CleanupOutbox(ctx, 24*time.Hour)
	if e != nil || deletedOutbox != 1 {
		t.Fatalf("expected 1 confirmed old outbox row deleted, got %d err=%v", deletedOutbox, e)
	}
	stillPending, e := f.r.PendingOutbox(ctx, 100, nil)
	if e != nil || len(stillPending) != len(pendingEvents)-1 {
		t.Fatalf("unsent outbox events lost during cleanup: want %d, got %d", len(pendingEvents)-1, len(stillPending))
	}

	// Advance 30s (less than 60s FrequencySeconds): outsider's fresh window stays, user's 25h-old window is deleted!
	f.now = f.now.Add(30 * time.Second)
	if next, e := f.s.CleanupFrequency(ctx, "g", ""); e != nil || next != "" {
		t.Fatalf("CleanupFrequency failed: next=%q err=%v", next, e)
	}
	if count(t, f, "g/frequency") != 1 {
		t.Fatalf("expected 1 active frequency window remaining, got %d", count(t, f, "g/frequency"))
	}
}

type denyMessageRead struct{ g.Authorizer }

func (a denyMessageRead) Evaluate(ctx context.Context, p account.Principal, cap string, scope admin.Scope) error {
	if cap == "groups.messages.read" {
		return admin.ErrForbidden
	}
	return a.Authorizer.Evaluate(ctx, p, cap, scope)
}
func TestForwardRetryRechecksCurrentReadPermission(t *testing.T) {
	f := setup(t)
	if err := f.s.Ingest(ctx, "g", event("forward-event", "qq-bot", "g", "groups.message", 1), g.Message{ID: "forwarded", Owner: user.Key(), Body: "fixture"}); err != nil {
		t.Fatal(err)
	}
	original := command(t, f, manager, "send-first", g.Command{Kind: "forward", Message: "forwarded"})
	f.bot.failure = true
	if action, err := f.s.Execute(ctx, "g", original.ID); err != nil || action.State != "failed" {
		t.Fatal(action, err)
	}
	auth := f.s.Auth
	f.s.Auth = denyMessageRead{auth}
	countBefore := count(t, f, "g/actions")
	retry := g.Command{Kind: "retry_action", Target: original.ID}
	if _, err := f.s.Command(ctx, manager, "g", "retry-denied", retry); !errors.Is(err, g.ErrForbidden) || count(t, f, "g/actions") != countBefore {
		t.Fatal(err)
	}
	f.s.Auth = auth
	newAction := command(t, f, manager, "retry-approved", retry)
	f.s.Auth = denyMessageRead{auth}
	calls := f.bot.calls
	act, err := f.s.Execute(ctx, "g", newAction.ID)
	if err != nil || act.State != "cancelled" || f.bot.calls != calls {
		t.Fatal(act, err)
	}
}
func TestSelfMuteRetryPreservesMemberConsent(t *testing.T) {
	f := setup(t)
	if err := f.s.MemberEvent(ctx, "g", event("join-play", "qq-bot", "g", "groups.member_joined", 1), user.Key()); err != nil {
		t.Fatal(err)
	}
	first := command(t, f, user, "member-consent", g.Command{Kind: "self_mute"})
	f.bot.failure = true
	if a, err := f.s.Execute(ctx, "g", first.ID); err != nil || a.State != "failed" {
		t.Fatal(a, err)
	}
	retry := command(t, f, manager, "retry-play", g.Command{Kind: "retry_action", Target: first.ID})
	f.bot.failure = false
	a, err := f.s.Execute(ctx, "g", retry.ID)
	if err != nil || a.State != "succeeded" || a.Actor.Principal() != user || a.Command.Target != user.Key() {
		t.Fatal(a, err)
	}
	page, err := f.s.Audits(ctx, manager, "g", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, audit := range page.Items {
		if audit.Action == "retry_action" && audit.Actor == manager.Key() && audit.Target == first.ID {
			return
		}
	}
	t.Fatal("missing administrator retry audit")
}
func TestCleanupRetainsConfirmedHeldOutbox(t *testing.T) {
	f := setup(t)
	events, err := f.r.PendingOutbox(ctx, 100, nil)
	if err != nil || len(events) == 0 {
		t.Fatal(events, err)
	}
	id := events[0].ID
	if err = f.r.Failed(ctx, id, f.now); err != nil {
		t.Fatal(err)
	}
	if err = f.r.Failed(ctx, id, f.now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = f.r.ConfirmOutbox(ctx, id); err != nil {
		t.Fatal(err)
	}
	var sent time.Time
	if err = f.pool.QueryRow(ctx, "SELECT sent_at FROM groups.outbox WHERE id=$1 AND held", id).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	f.now = sent.Add(48 * time.Hour)
	if _, err = f.s.CleanupOutbox(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err = f.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM groups.outbox WHERE id=$1 AND held)", id).Scan(&retained); err != nil || !retained {
		t.Fatal(retained, err)
	}
}
