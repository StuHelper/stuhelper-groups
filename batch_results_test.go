package groups_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/sdk"
)

func putBatchMember(t *testing.T, f *fixture, target, state string, joined bool) {
	t.Helper()
	if err := f.r.Transaction(ctx, func(tx g.Tx) error {
		return tx.Put("g/members", target, g.Member{Owner: target, State: state, Joined: joined})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBatchSkipsIneligibleMembersAndReplaysResults(t *testing.T) {
	for _, cmd := range []map[string]any{
		{"kind": "warn", "text": "reason"}, {"kind": "mute", "seconds": 60}, {"kind": "unmute"}, {"kind": "kick"},
	} {
		t.Run(cmd["kind"].(string), func(t *testing.T) {
			f := setup(t)
			putBatchMember(t, f, "first", "restored", true)
			putBatchMember(t, f, "applicant", "awaiting_verification", false)
			putBatchMember(t, f, "removed", "removed", true)
			putBatchMember(t, f, "last", "restored", true)
			targets := []string{"missing", "first", "applicant", "removed", "last"}
			submit := func(preview bool) sdk.BulkResult {
				t.Helper()
				w := api(t, f, manager, "POST", "/api/v1/groups/g/batch", "mixed", map[string]any{
					"targets": targets, "command": cmd, "preview": preview,
				})
				var result sdk.BulkResult
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
					t.Fatal(w.Code, w.Body.String())
				}
				return result
			}
			before := databaseDigest(t, f)
			preview := submit(true)
			if databaseDigest(t, f) != before {
				t.Fatal("preview changed the database")
			}
			beforeAudit := count(t, f, "g/audit")
			actual := submit(false)
			if n := count(t, f, "g/actions"); n != 2 {
				t.Fatalf("queued %d actions, want only the two joined targets", n)
			}
			if count(t, f, "g/audit") != beforeAudit+2 {
				t.Fatal("expected one execution audit per queued target")
			}
			wantSkipped := []sdk.BulkSkippedItem{
				{Target: "missing", ReasonCode: "not_found", Reason: "无成员记录"},
				{Target: "applicant", ReasonCode: "not_joined", Reason: "未入群"},
				{Target: "removed", ReasonCode: "not_joined", Reason: "未入群"},
			}
			for _, result := range []sdk.BulkResult{preview, actual} {
				if !reflect.DeepEqual(result.Skipped, wantSkipped) || len(result.Executed) != 2 || len(result.Items) != len(targets) {
					t.Fatal(result)
				}
				for i, item := range result.Items {
					if item.Target != targets[i] {
						t.Fatal(result.Items)
					}
				}
			}
			for i, target := range []string{"first", "last"} {
				item := actual.Executed[i]
				if preview.Executed[i].Target != target || preview.Executed[i].Result.State != "would_queue" || item.Target != target || item.Result.State != "pending" {
					t.Fatal(preview.Executed, actual.Executed)
				}
				action, err := f.s.Execute(ctx, "g", item.Result.Id)
				if err != nil || action.State != "succeeded" || action.Command.Target != target || action.Command.Kind != cmd["kind"] {
					t.Fatal(action, err)
				}
			}
			if f.bot.calls != 2 {
				t.Fatalf("bot calls = %d", f.bot.calls)
			}

			putBatchMember(t, f, "first", "removed", false)
			putBatchMember(t, f, "missing", "restored", true)
			putBatchMember(t, f, "applicant", "restored", true)
			before = databaseDigest(t, f)
			if replay := submit(false); !reflect.DeepEqual(actual, replay) || databaseDigest(t, f) != before {
				t.Fatal("retry changed the result or produced additional writes", replay)
			}
		})
	}
}

func TestBatchChecksCurrentMembershipAndHandlesConcurrentRetries(t *testing.T) {
	f := setup(t)
	putBatchMember(t, f, "left", "restored", true)
	in := g.Bulk{Command: g.Command{Kind: "mute", Seconds: 60}, Targets: []string{"left", "joined"}, Preview: true}
	preview, err := f.s.Batch(ctx, manager, "g", "changed", in)
	if err != nil || len(preview.Executed) != 1 || preview.Executed[0].Target != "left" {
		t.Fatal(preview, err)
	}
	putBatchMember(t, f, "left", "removed", false)
	putBatchMember(t, f, "joined", "restored", true)
	in.Preview = false
	type response struct {
		out g.BulkResult
		err error
	}
	responses := make(chan response, 4)
	for range cap(responses) {
		go func() {
			out, err := f.s.Batch(ctx, manager, "g", "changed", in)
			responses <- response{out, err}
		}()
	}
	var first g.BulkResult
	for i := range cap(responses) {
		r := <-responses
		if r.err != nil || len(r.out.Executed) != 1 || r.out.Executed[0].Target != "joined" || len(r.out.Skipped) != 1 || r.out.Skipped[0].Target != "left" || r.out.Skipped[0].ReasonCode != "not_joined" {
			t.Fatal(r)
		}
		if i == 0 {
			first = r.out
		} else if !reflect.DeepEqual(first, r.out) {
			t.Fatal("concurrent submissions returned different outcomes", first, r.out)
		}
	}
	if count(t, f, "g/actions") != 1 {
		t.Fatal("concurrent retry duplicated the mute action")
	}
	f.revoked.Store(true)
	if _, err := f.s.Batch(ctx, manager, "g", "changed", in); !errors.Is(err, g.ErrForbidden) {
		t.Fatal("revoked manager could replay results", err)
	}
}

func TestBatchRollsBackRejectedTargetBeforeSavingReason(t *testing.T) {
	f := setup(t)
	cfg := config()
	cfg.RemindCard, cfg.AutoCard = true, true
	if _, err := configureCurrent(t, f, manager, "card-setup", cfg); err != nil {
		t.Fatal(err)
	}
	// The reminder fits, but the subsequent card update exceeds the queue limit.
	if err := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := range 999 {
			id := fmt.Sprintf("pending-%d", i)
			if err := tx.Put("g/actions", id, g.Action{ID: id, Group: "g", State: "pending"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in := g.Bulk{Command: g.Command{Kind: "card", Text: "old"}, Targets: []string{user.Key()}, Preview: true}
	preview, err := f.s.Batch(ctx, manager, "g", "capacity", in)
	if err != nil || len(preview.Skipped) != 1 || preview.Skipped[0].ReasonCode != "action_queue_full" || preview.Skipped[0].Reason == "" {
		t.Fatal(preview, err)
	}
	in.Preview = false
	before := count(t, f, "g/audit")
	actual, err := f.s.Batch(ctx, manager, "g", "capacity", in)
	if err != nil || actual.Executed == nil || len(actual.Executed) != 0 || !reflect.DeepEqual(preview.Skipped, actual.Skipped) || count(t, f, "g/actions") != 999 || before != count(t, f, "g/audit") {
		t.Fatal(actual, err)
	}
	if err := f.r.Transaction(ctx, func(tx g.Tx) error { return tx.Delete("g/actions", "pending-0") }); err != nil {
		t.Fatal(err)
	}
	digest := databaseDigest(t, f)
	if again, err := f.s.Batch(ctx, manager, "g", "capacity", in); err != nil || !reflect.DeepEqual(again, actual) || databaseDigest(t, f) != digest {
		t.Fatal(again, err)
	}
}
