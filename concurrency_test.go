package groups_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
)

type blockingAI struct {
	started chan time.Duration
	release chan struct{}
}

func (b *blockingAI) Analyze(ctx context.Context, _ string) (string, []string, error) {
	deadline, ok := ctx.Deadline()
	var remaining time.Duration
	if ok {
		remaining = time.Until(deadline)
	}
	select {
	case b.started <- remaining:
	default:
	}
	select {
	case <-b.release:
		return "AI建议禁言", []string{"mute"}, nil
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
}

func TestReadOnlyLockFreePreFetchAndCrossGroupIsolation(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}

	g2 := config()
	g2.ID = "g2"
	g2.School = "school-2"
	if _, e := f.s.Configure(ctx, root, "cfg-g2", g2); e != nil {
		t.Fatal(e)
	}

	// Prepare retained message and two open reports in group g.
	if e := f.s.Ingest(ctx, "g", event("m-conc", "qq-bot", "g", "groups.message", 1), g.Message{ID: "m-conc", Owner: user.Key(), Body: "test"}); e != nil {
		t.Fatal(e)
	}
	rep1 := command(t, f, user, "rep-c1", g.Command{Kind: "report", Message: "m-conc", Text: "spam"})
	rep2 := command(t, f, user, "rep-c2", g.Command{Kind: "report", Message: "m-conc", Text: "ad"})

	aiGate := &blockingAI{started: make(chan time.Duration, 2), release: make(chan struct{})}
	f.s.AI = aiGate

	analyzeErr := make(chan error, 1)
	go func() {
		_, err := f.s.Command(ctx, manager, "g", "analyze-c1", g.Command{Kind: "analyze", Target: rep1.ID})
		analyzeErr <- err
	}()

	var timeoutRemaining time.Duration
	select {
	case timeoutRemaining = <-aiGate.started:
	case <-time.After(3 * time.Second):
		t.Fatal("analyze did not reach external AI")
	}
	// AI.Analyze must have its own ~5s timeout rather than the 10s DB transaction timeout.
	if timeoutRemaining <= 0 || timeoutRemaining > 5500*time.Millisecond {
		t.Fatalf("expected ~5s AI timeout, got %v", timeoutRemaining)
	}

	// While AI.Analyze is blocked in-flight:
	// 1. ReadOnly queries complete immediately without waiting on advisory locks.
	readCtx, cancelRead := context.WithTimeout(ctx, 2*time.Second)
	defer cancelRead()
	if _, e := f.s.Configuration(readCtx, manager, "g"); e != nil {
		t.Fatalf("Configuration blocked during AI call: %v", e)
	}
	if _, e := f.s.Groups(readCtx, manager, "", 10); e != nil {
		t.Fatalf("Groups blocked during AI call: %v", e)
	}
	if _, e := f.s.Members(readCtx, manager, "g", "", 10); e != nil {
		t.Fatalf("Members blocked during AI call: %v", e)
	}
	if _, e := f.s.MyGroups(readCtx, user, "", 10); e != nil {
		t.Fatalf("MyGroups blocked during AI call: %v", e)
	}
	if _, e := f.s.GroupsForOwner(readCtx, user, "", 10); e != nil {
		t.Fatalf("GroupsForOwner blocked during AI call: %v", e)
	}

	// 2. Writes on another group g2 AND writes on group g complete without waiting on AI.Analyze.
	if _, e := f.s.Command(readCtx, root, "g2", "warn-g2", g.Command{Kind: "warn", Target: user.Key(), Text: "independent"}); e != nil {
		t.Fatalf("g2 write blocked by g analyze: %v", e)
	}
	// Resolve rep1 concurrently before AI finishes: post-AI state check must detect resolution and return ErrConflict.
	if _, e := f.s.Command(readCtx, manager, "g", "resolve-rep1", g.Command{Kind: "feedback", Target: rep1.ID, Text: "already_handled"}); e != nil {
		t.Fatalf("concurrent feedback blocked by AI call: %v", e)
	}

	close(aiGate.release)
	if err := <-analyzeErr; !errors.Is(err, g.ErrConflict) {
		t.Fatalf("expected ErrConflict when report was resolved during AI analysis, got %v", err)
	}

	// Normal lock-free AI analysis on rep2 succeeds and replays without re-invoking AI.
	aiGate2 := &blockingAI{started: make(chan time.Duration, 2), release: make(chan struct{})}
	close(aiGate2.release)
	f.s.AI = aiGate2
	res, e := f.s.Command(ctx, manager, "g", "analyze-c2", g.Command{Kind: "analyze", Target: rep2.ID})
	if e != nil || res.Detail != "AI建议禁言" {
		t.Fatalf("analyze failed: %+v %v", res, e)
	}
	f.s.AI = nil // Replay must succeed from stored receipt without calling AI again.
	replayed, e := f.s.Command(ctx, manager, "g", "analyze-c2", g.Command{Kind: "analyze", Target: rep2.ID})
	if e != nil || replayed.Detail != res.Detail {
		t.Fatalf("analyze replay invoked external AI or changed result: %+v %v", replayed, e)
	}
}

func TestSweepPreFetchesFactsOutsideLockAndMultiGroupLocksNeverDeadlock(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}

	for _, id := range []string{"g-alpha", "g-beta", "g-gamma"} {
		c := config()
		c.ID = id
		c.School = "school-" + id
		if _, e := f.s.Configure(ctx, root, "init-"+id, c); e != nil {
			t.Fatal(e)
		}
	}
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		return tx.Put("g-alpha/members", user.Key(), g.Member{Owner: user.Key(), Joined: true, State: "restored"})
	}); e != nil {
		t.Fatal(e)
	}

	// Block GroupFacts for school-g-alpha during Sweep; verify writes to g-beta and reads do not block.
	factsStarted := make(chan struct{}, 1)
	releaseFacts := make(chan struct{})
	f.s.Facts = groupFactsFunc(func(_ context.Context, _ account.Principal, _, school string) (g.Facts, error) {
		if school == "school-g-alpha" {
			select {
			case factsStarted <- struct{}{}:
			default:
			}
			<-releaseFacts
		}
		return g.Facts{Bound: true, StudentKnown: true, Student: true, StageKnown: true}, nil
	})

	sweepDone := make(chan error, 1)
	go func() {
		_, err := f.s.Sweep(ctx, "g-alpha", "")
		sweepDone <- err
	}()
	select {
	case <-factsStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("Sweep did not reach GroupFacts")
	}

	opCtx, cancelOp := context.WithTimeout(ctx, 2*time.Second)
	defer cancelOp()
	if _, e := f.s.Command(opCtx, root, "g-beta", "beta-warn", g.Command{Kind: "warn", Target: user.Key(), Text: "unblocked"}); e != nil {
		t.Fatalf("g-beta write blocked while g-alpha Sweep awaited GroupFacts: %v", e)
	}
	if _, e := f.s.Groups(opCtx, root, "", 10); e != nil {
		t.Fatalf("Groups read blocked while g-alpha Sweep awaited GroupFacts: %v", e)
	}
	close(releaseFacts)
	if err := <-sweepDone; err != nil {
		t.Fatalf("Sweep failed: %v", err)
	}

	// Concurrent multi-group Blacklists in opposite group orders must never deadlock.
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	orders := [][]string{
		{"g-alpha", "g-beta", "g-gamma"},
		{"g-gamma", "g-beta", "g-alpha"},
		{"g-beta", "g-gamma", "g-alpha"},
	}
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.s.Blacklists(ctx, root, fmt.Sprintf("multi-blk-%d", i), g.BlacklistInput{
				Groups: orders[i%len(orders)],
				Target: fmt.Sprintf("target-%d", i%4),
				Reason: "concurrent-multi-group",
				Remove: i%5 == 0,
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("multi-group concurrent transaction failed or deadlocked: %v", err)
		}
	}
}
