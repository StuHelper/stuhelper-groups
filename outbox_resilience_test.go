package groups_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
)

type togglingBot struct {
	calls     int
	onExecute func()
}

func (b *togglingBot) Execute(_ context.Context, _ g.Action) (g.Outcome, error) {
	b.calls++
	if b.onExecute != nil {
		b.onExecute()
	}
	return g.Outcome{Success: true, Code: "ok"}, nil
}

func (b *togglingBot) Lookup(_ context.Context, _ string) (g.Outcome, bool, error) {
	return g.Outcome{}, false, nil
}

func TestFanoutReceiverFailuresDoNotBlockHealthyTargetsOrSourceOutbox(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}

	for _, id := range []string{"r-disabled", "r-full", "r-healthy"} {
		c := config()
		c.ID = id
		if _, err := f.s.Configure(ctx, root, "cfg-"+id, c); err != nil {
			t.Fatal(err)
		}
		command(t, f, root, "sub-"+id, g.Command{Kind: "subscribe", Receiver: id, Enabled: true})
	}
	// Disable bot on r-disabled, fill r-full to 1000 pending actions, and inject a missing receiver subscription.
	disabledCfg := config()
	disabledCfg.ID = "r-disabled"
	disabledCfg.BotEnabled = false
	if _, err := configureCurrent(t, f, root, "disable-r", disabledCfg); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 0; i < 1000; i++ {
			id := fmt.Sprintf("fill-%04d", i)
			if err := tx.Put("r-full/actions", id, g.Action{ID: id, Group: "r-full", State: "pending"}); err != nil {
				return err
			}
		}
		return tx.Put("g/subscriptions", "r-missing", map[string]any{"receiver": "r-missing", "enabled": true, "actor": g.Actor(root)})
	}); err != nil {
		t.Fatal(err)
	}

	r := command(t, f, manager, "warn-fanout", g.Command{Kind: "warn", Target: user.Key(), Text: "violation"})
	if _, err := f.s.Execute(ctx, "g", r.ID); err != nil {
		t.Fatal(err)
	}
	if count(t, f, "r-disabled/actions") != 0 {
		t.Fatal("disabled receiver got an action before fanout")
	}
	var raw []byte
	if err := f.pool.QueryRow(ctx, "SELECT envelope FROM groups.outbox WHERE envelope->>'type'='groups.action.result' ORDER BY sequence DESC LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ev g.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}

	if err := f.s.Fanout(ctx, ev); err != nil {
		t.Fatalf("Fanout blocked source group outbox on single-receiver rejection: %v", err)
	}

	if count(t, f, "r-disabled/actions") != 0 || count(t, f, "r-full/actions") != 1000 {
		t.Fatal("rejected delivery wrote an action to the receiver")
	}
	healthyActions, err := f.s.Actions(ctx, root, "r-healthy", "", 100)
	if err != nil || len(healthyActions.Items) != 1 || healthyActions.Items[0].State != "pending" {
		t.Fatalf("healthy receiver did not get queued action: %+v err=%v", healthyActions, err)
	}

	deliveries, err := f.s.Deliveries(ctx, manager, "g", "", 100)
	if err != nil || len(deliveries.Items) != 4 {
		t.Fatalf("expected 4 delivery records, got %+v err=%v", deliveries, err)
	}
	byReceiver := map[string]string{}
	for _, d := range deliveries.Items {
		byReceiver[d.Receiver] = d.State
	}
	if byReceiver["r-disabled"] != "cancelled" || byReceiver["r-full"] != "failed" || byReceiver["r-missing"] != "failed" || byReceiver["r-healthy"] != "pending" {
		t.Fatalf("unexpected independent delivery states: %+v", byReceiver)
	}
}

func TestExecuteRevocationCancelsActionAndFinishPreservesExecutedWarn(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}

	// 1. Revoked actor permission cancels pending action and frees capacity without returning outbox-blocking error.
	queued := command(t, f, manager, "revoked-kick", g.Command{Kind: "kick", Target: user.Key()})
	f.revoked.Store(true)
	cancelled, err := f.s.Execute(ctx, "g", queued.ID)
	if err != nil || cancelled.State != "cancelled" || cancelled.Result != "groups_forbidden" || f.bot.calls != 0 {
		t.Fatalf("revoked action not cancelled cleanly: action=%+v err=%v calls=%d", cancelled, err, f.bot.calls)
	}
	var pending int
	if err := f.r.Transaction(ctx, func(tx g.Tx) error {
		var e error
		pending, e = tx.PendingActions("g")
		return e
	}); err != nil || pending != 0 {
		t.Fatalf("cancelled action still counted as pending: pending=%d err=%v", pending, err)
	}
	f.revoked.Store(false)

	// 2. Warning limit auto-mute blocked during finish (e.g. bot disabled mid-flight) must not roll back already-executed warn.
	cfg := config()
	cfg.WarningLimit = 1
	cfg.AutoActions = []string{"mute"}
	cfg.MuteSeconds = 60
	if _, err := configureCurrent(t, f, manager, "enable-warn-limit", cfg); err != nil {
		t.Fatal(err)
	}
	warnRes := command(t, f, manager, "warn-limit-hit", g.Command{Kind: "warn", Target: user.Key(), Text: "hit limit"})

	tb := &togglingBot{}
	tb.onExecute = func() {
		// While external bot call is in flight, admin disables bot (or revokes manager).
		disable := cfg
		disable.BotEnabled = false
		if _, e := configureCurrent(t, f, root, "disable-mid-flight", disable); e != nil {
			t.Errorf("disable mid-flight failed: %v", e)
		}
	}
	f.s.Bot = tb

	done, err := f.s.Execute(ctx, "g", warnRes.ID)
	if err != nil || done.State != "succeeded" || tb.calls != 1 {
		t.Fatalf("already-executed warn rolled back on derived auto-mute rejection: action=%+v err=%v calls=%d", done, err, tb.calls)
	}
	warnings, err := f.s.Warnings(ctx, root, "g", "", 100)
	if err != nil || len(warnings.Items) != 1 || warnings.Items[0].Count != 1 {
		t.Fatalf("warning count lost on finish: %+v err=%v", warnings, err)
	}
}

func TestUnknownWarningAfterRevocationUsesActualResult(t *testing.T) {
	f := setup(t)
	r := command(t, f, manager, "warn", g.Command{Kind: "warn", Text: "reason", Target: user.Key()})
	f.bot.timeout = true
	if a, err := f.s.Execute(ctx, "g", r.ID); err == nil || a.State != "unknown" {
		t.Fatal(a, err)
	}
	f.revoked.Store(true)
	a, err := f.s.Execute(ctx, "g", r.ID)
	if err != nil || a.State != "succeeded" || f.bot.calls != 1 || count(t, f, "g/warnings") != 1 {
		t.Fatal(a, err)
	}
}
