package groups_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
)

func upstream(id, source, subject string, owner account.Principal, revision int64) g.UpstreamEvent {
	kind := "verification.credential.activated.v1"
	if source == "qq-binding" {
		kind = "qq-binding.unlink.onebot.v1"
	}
	return g.UpstreamEvent{SpecVersion: "1.0", ID: id, Source: source, Subject: subject, Type: kind, Time: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), DataContentType: "application/json", Data: g.EligibilityChange{Owner: owner.Key(), Revision: revision}}
}
func seedMembership(t *testing.T, f *fixture, owner account.Principal, total int) {
	t.Helper()
	e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 0; i < total; i++ {
			id := fmt.Sprintf("owned-%04d", i)
			input := config()
			input.ID = id
			input.School = fmt.Sprintf("school-%d", i%2)
			if e := tx.Put("config", id, g.GroupConfig{GroupSettings: input.GroupSettings, Revision: 1}); e != nil {
				return e
			}
			if e := tx.Put(id+"/members", owner.Key(), g.Member{Owner: owner.Key(), Joined: true, State: "restored"}); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}
func member(t *testing.T, f *fixture, group string, owner account.Principal) g.Member {
	t.Helper()
	var m g.Member
	if e := f.r.Transaction(ctx, func(tx g.Tx) error { return tx.Get(group+"/members", owner.Key(), &m) }); e != nil {
		t.Fatal(e)
	}
	return m
}
func inboxCount(t *testing.T, f *fixture, id string) int {
	t.Helper()
	var n int
	if e := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.inbox WHERE id=$1", id).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func TestPendingOutboxExclusionLimitOrderingAndConfirmation(t *testing.T) {
	f := setup(t)
	e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 0; i < 105; i++ {
			v := event(fmt.Sprintf("blocked-%03d", i), "groups", "a-blocked", "groups.test", int64(i+1))
			if e := tx.Emit(v); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	// Insertion order deliberately differs from revision and ID order.
	for _, v := range []g.Event{event("z", "groups", "b-ready", "groups.test", 10), event("y", "groups", "b-ready", "groups.test", 2), event("x", "groups", "b-ready", "groups.test", 2), event("w", "groups", "c-ready", "groups.test", 1)} {
		data, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.pool.Exec(ctx, "INSERT INTO groups.outbox(id,source,subject,envelope,created_at) VALUES($1,$2,$3,$4,$5)", v.ID, v.Source, v.Subject, data, v.Time); e != nil {
			t.Fatal(e)
		}
	}
	baseline, e := f.r.PendingOutbox(ctx, 100, nil)
	if e != nil || len(baseline) != 100 {
		t.Fatal(len(baseline), e)
	}
	for _, v := range baseline {
		if v.Subject != "a-blocked" {
			t.Fatal("fixture failed to fill blocked page", v)
		}
	}
	again, e := f.r.PendingOutbox(ctx, 100, []string{})
	if e != nil || !reflect.DeepEqual(again, baseline) {
		t.Fatal("read mutated outbox or nil differs", e)
	}
	excluded := []string{"a-blocked", "g"}
	for len(excluded) < 1000 {
		excluded = append(excluded, fmt.Sprint("absent-", len(excluded)))
	}
	rows, e := f.r.PendingOutbox(ctx, 100, excluded)
	if e != nil {
		t.Fatal(e)
	}
	ids := []string{}
	for _, v := range rows {
		ids = append(ids, v.ID)
	}
	if !reflect.DeepEqual(ids, []string{"x", "y", "z", "w"}) {
		t.Fatal(ids)
	}
	f.r.Now = func() time.Time { return f.now }
	if e = f.r.ConfirmOutbox(ctx, "x"); e != nil {
		t.Fatal(e)
	}
	var first time.Time
	if e = f.pool.QueryRow(ctx, "SELECT sent_at FROM groups.outbox WHERE id='x'").Scan(&first); e != nil {
		t.Fatal(e)
	}
	f.now = f.now.Add(time.Hour)
	if e = f.r.ConfirmOutbox(ctx, "x"); e != nil {
		t.Fatal(e)
	}
	var second time.Time
	if e = f.pool.QueryRow(ctx, "SELECT sent_at FROM groups.outbox WHERE id='x'").Scan(&second); e != nil || !first.Equal(second) {
		t.Fatal("confirmation changed timestamp", e)
	}
	rows, e = f.r.PendingOutbox(ctx, 1, []string{"a-blocked", "c-ready", "g", "a-blocked"})
	if e != nil || len(rows) != 1 || rows[0].ID != "y" {
		t.Fatal(rows, e)
	}
	for _, limit := range []int{-1, 0, 101} {
		if _, e = f.r.PendingOutbox(ctx, limit, nil); !errors.Is(e, g.ErrInvalid) {
			t.Fatal(e)
		}
	}
	if _, e = f.r.PendingOutbox(ctx, 100, append(excluded, "extra")); !errors.Is(e, g.ErrInvalid) {
		t.Fatal(e)
	}
	rows, e = f.r.PendingOutbox(ctx, 100, []string{"a-blocked", "b-ready", "c-ready", "g"})
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	if e = f.r.ConfirmOutbox(ctx, "missing"); !errors.Is(e, g.ErrNotFound) {
		t.Fatal(e)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = f.r.PendingOutbox(canceled, 1, nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e = f.r.ConfirmOutbox(canceled, "y"); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestGroupsForOwnerPaginationAndBounds(t *testing.T) {
	f := setup(t)
	seedMembership(t, f, user, 205)
	e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for _, id := range []string{"removed", "applicant", "someone-else"} {
			in := config()
			in.ID = id
			if e := tx.Put("config", id, g.GroupConfig{GroupSettings: in.GroupSettings, Revision: 1}); e != nil {
				return e
			}
		}
		if e := tx.Put("removed/members", user.Key(), g.Member{Owner: user.Key(), Joined: true, State: "removed"}); e != nil {
			return e
		}
		if e := tx.Put("applicant/members", user.Key(), g.Member{Owner: user.Key(), State: "awaiting_verification"}); e != nil {
			return e
		}
		if e := tx.Put("someone-else/members", outsider.Key(), g.Member{Owner: outsider.Key(), Joined: true, State: "restored"}); e != nil {
			return e
		}
		return tx.Put("unmanaged/members", user.Key(), g.Member{Owner: user.Key(), Joined: true, State: "restored"})
	})
	if e != nil {
		t.Fatal(e)
	}
	cursor := ""
	ids := []string{}
	for _, want := range []int{100, 100, 5} {
		page, e := f.s.GroupsForOwner(ctx, user, cursor, 100)
		if e != nil || len(page.Items) != want {
			t.Fatal(page, e)
		}
		for _, v := range page.Items {
			ids = append(ids, v.ID)
			if v.School == "" {
				t.Fatal(v)
			}
		}
		cursor = page.NextCursor
	}
	if cursor != "" || len(ids) != 205 {
		t.Fatal(cursor, len(ids))
	}
	for i, id := range ids {
		if id != fmt.Sprintf("owned-%04d", i) {
			t.Fatal(ids)
		}
	}
	for _, limit := range []int{0, 101, -1} {
		if _, e = f.s.GroupsForOwner(ctx, user, "", limit); !errors.Is(e, g.ErrInvalid) {
			t.Fatal(e)
		}
	}
	if _, e = f.s.GroupsForOwner(ctx, account.Principal{}, "", 10); !errors.Is(e, g.ErrInvalid) {
		t.Fatal(e)
	}
	sameSubjectOtherIssuer := account.Principal{Issuer: "other", Subject: user.Subject}
	page, e := f.s.GroupsForOwner(ctx, sameSubjectOtherIssuer, "", 10)
	if e != nil || len(page.Items) != 0 {
		t.Fatal("issuer ignored", page, e)
	}
	page, e = f.s.GroupsForOwner(ctx, user, "owned-0204", 10)
	if e != nil || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatal(page, e)
	}
}

func TestConsumeFanoutDeduplicationOldRevisionsAndConcurrency(t *testing.T) {
	f := setup(t)
	seedMembership(t, f, user, 105)
	f.facts.set(false)
	var calls atomic.Int64
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		calls.Add(1)
		return g.Facts{Bound: true, StudentKnown: true, StageKnown: true, Student: false}, nil
	})
	ev := upstream("changed", "verification", "credential/1", user, 2)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.s.Consume(ctx, ev) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if calls.Load() != 105 || inboxCount(t, f, ev.ID) != 1 {
		t.Fatal("duplicate recomputation", calls.Load())
	}
	for i := 0; i < 105; i++ {
		id := fmt.Sprintf("owned-%04d", i)
		m := member(t, f, id, user)
		if m.State != "grace" || m.Deadline != f.now.Add(time.Hour) || count(t, f, id+"/actions") != 1 {
			t.Fatalf("%s %+v", id, m)
		}
	}
	old := upstream("old", "verification", "credential/1", user, 1)
	if e := f.s.Consume(ctx, old); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 105 || inboxCount(t, f, old.ID) != 1 {
		t.Fatal("old revision executed")
	}
	equal := upstream("same-revision", "verification", "credential/1", user, 2)
	if e := f.s.Consume(ctx, equal); e != nil || calls.Load() != 105 {
		t.Fatal("equal revision executed", e)
	}
	conflict := ev
	conflict.Data.Owner = outsider.Key()
	if e := f.s.Consume(ctx, conflict); !errors.Is(e, g.ErrConflict) {
		t.Fatal(e)
	}
	// Revisions are per upstream subject, not per owner. Another credential must run.
	other := upstream("other-object", "verification", "credential/2", user, 1)
	if e := f.s.Consume(ctx, other); e != nil || calls.Load() != 210 {
		t.Fatal("object revision collapsed into owner", e, calls.Load())
	}
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		calls.Add(1)
		return g.Facts{Bound: true, StudentKnown: true, StageKnown: true, Student: true}, nil
	})
	restored := upstream("binding-restored", "qq-binding", "binding/owner", user, 1)
	if e := f.s.Consume(ctx, restored); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 105; i++ {
		m := member(t, f, fmt.Sprintf("owned-%04d", i), user)
		if m.State != "restored" || !m.Deadline.IsZero() {
			t.Fatal(m)
		}
	}
}

func TestConsumeFailureRollsBackInboxAllGroupsAndOutbox(t *testing.T) {
	f := setup(t)
	seedMembership(t, f, user, 2)
	calls := 0
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		calls++
		if calls == 2 {
			return g.Facts{}, g.ErrUnavailable
		}
		return g.Facts{Bound: false, StudentKnown: true, StageKnown: true}, nil
	})
	ev := upstream("failure", "qq-binding", "binding/owner", user, 1)
	var before int
	if e := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.outbox").Scan(&before); e != nil {
		t.Fatal(e)
	}
	if e := f.s.Consume(ctx, ev); !errors.Is(e, g.ErrUnavailable) {
		t.Fatal(e)
	}
	if inboxCount(t, f, ev.ID) != 0 || count(t, f, "owned-0000/actions") != 0 || count(t, f, "owned-0000/audit") != 0 {
		t.Fatal("partial failure committed")
	}
	for i := 0; i < 2; i++ {
		m := member(t, f, fmt.Sprintf("owned-%04d", i), user)
		if m.State != "restored" || !m.Deadline.IsZero() {
			t.Fatal(m)
		}
	}
	var after, revision int
	if e := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.outbox").Scan(&after); e != nil || before != after {
		t.Fatal("outbox survived rollback", e)
	}
	if e := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.revisions WHERE source='qq-binding' AND subject='binding/owner'").Scan(&revision); e != nil || revision != 0 {
		t.Fatal("revision survived rollback", e)
	}
	f.s.Facts = f.facts
	f.facts.set(false)
	if e := f.s.Consume(ctx, ev); e != nil {
		t.Fatal(e)
	}
	if inboxCount(t, f, ev.ID) != 1 || member(t, f, "owned-0000", user).State != "grace" {
		t.Fatal("failed event could not retry")
	}
}

func TestConsumeApplicantsNoMembershipInvalidAndBoundedFanout(t *testing.T) {
	f := setup(t)
	f.facts.set(false)
	command(t, f, user, "apply", g.Command{Kind: "apply"})
	f.facts.set(true)
	ev := upstream("approved", "verification", "credential/1", user, 1)
	if e := f.s.Consume(ctx, ev); e != nil {
		t.Fatal(e)
	}
	if m := member(t, f, "g", user); m.State != "restored" || m.Joined {
		t.Fatal("applicant not reevaluated, or fake join", m)
	}
	foundApproval := false
	for _, a := range actions(t, f) {
		if a.Command.Kind == "approve" {
			foundApproval = true
		}
	}
	if !foundApproval {
		t.Fatal("no admission approval")
	}
	empty := upstream("no-groups", "verification", "credential/empty", outsider, 1)
	if e := f.s.Consume(ctx, empty); e != nil || inboxCount(t, f, empty.ID) != 1 {
		t.Fatal(e)
	}
	for _, mutate := range []func(*g.UpstreamEvent){func(e *g.UpstreamEvent) { e.Source = "evil" }, func(e *g.UpstreamEvent) { e.Type = "verification.manual.rejected.v1" }, func(e *g.UpstreamEvent) { e.DataContentType = "" }, func(e *g.UpstreamEvent) { e.Data.Owner = "invalid" }, func(e *g.UpstreamEvent) { e.Data.Revision = 0 }} {
		bad := ev
		bad.ID = "invalid"
		mutate(&bad)
		if e := f.s.Consume(ctx, bad); !errors.Is(e, g.ErrInvalid) || inboxCount(t, f, bad.ID) != 0 {
			t.Fatal(e)
		}
	}
	seedMembership(t, f, outsider, g.MaxEventGroups+1)
	overflow := upstream("overflow", "verification", "credential/many", outsider, 1)
	if e := f.s.Consume(ctx, overflow); !errors.Is(e, g.ErrUnavailable) || inboxCount(t, f, overflow.ID) != 0 {
		t.Fatal("unbounded or partial fanout", e)
	}
	if m := member(t, f, "owned-0000", outsider); m.State != "restored" {
		t.Fatal(m)
	}
}

func TestRelayStopsFailedSubjectWithinBatchAndDeliversOthers(t *testing.T) {
	f := setup(t)
	f.r.Now = func() time.Time { return f.now }
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 0; i < 20; i++ {
			if e := tx.Emit(event(fmt.Sprintf("a-%02d", i), "groups", "a-blocked", "groups.test", int64(i+1))); e != nil {
				return e
			}
		}
		return tx.Emit(event("healthy", "groups", "z-healthy", "groups.test", 1))
	}); e != nil {
		t.Fatal(e)
	}
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var calls atomic.Int64
	healthy := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- f.r.Relay(relayCtx, func(_ context.Context, e g.Event) error {
			if e.Subject == "a-blocked" {
				calls.Add(1)
				return errors.New("retry")
			}
			if e.ID == "healthy" {
				healthy <- struct{}{}
			}
			return nil
		})
	}()
	select {
	case <-healthy:
	case <-time.After(2 * time.Second):
		t.Fatal("healthy subject blocked")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, e := f.r.PendingOutbox(ctx, 100, []string{"a-blocked"})
		if e != nil {
			t.Fatal(e)
		}
		if len(rows) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("healthy subject not confirmed")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not stop")
	}
	if calls.Load() != 1 {
		t.Fatal("failed stream overtook first event", calls.Load())
	}
	pending, e := f.r.PendingOutbox(ctx, 100, nil)
	if e != nil || len(pending) != 20 {
		t.Fatal("failed subject confirmed", len(pending), e)
	}
}
