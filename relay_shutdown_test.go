package groups_test

import (
	"context"
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"testing"
	"time"
)

func TestRelayShutdownAndFirstFailure(t *testing.T) {
	f := setup(t)
	events, err := f.r.PendingOutbox(ctx, 1, nil)
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	id := events[0].ID
	now := f.now.Add(48 * time.Hour)
	if err = f.r.Failed(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	var held bool
	if err = f.pool.QueryRow(ctx, "SELECT held FROM groups.outbox WHERE id=$1", id).Scan(&held); err != nil || held {
		t.Fatal("first failure held", err)
	}
	if err = f.r.Retry(ctx, id, f.now); err != nil {
		t.Fatal(err)
	}
	stop, cancel := context.WithCancel(ctx)
	defer cancel()
	err = f.r.Relay(stop, func(context.Context, g.Event) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	events, err = f.r.PendingOutbox(ctx, 1, nil)
	if err != nil || len(events) != 0 {
		t.Fatalf("unconfirmed event: %v %v", events, err)
	}
}
