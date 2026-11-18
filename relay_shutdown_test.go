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

func TestMaintenanceErrorKeepsPreviousPage(t *testing.T) {
	f := setup(t)
	if err := f.r.Transaction(ctx, func(tx g.Tx) error {
		if err := tx.Put("g/messages", "a", g.Message{ID: "a", Expires: f.now}); err != nil {
			return err
		}
		for _, bucket := range []string{"members", "actions", "messages"} {
			if err := tx.Put("g/"+bucket, "b", "invalid existing record"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, job := range []func(context.Context, string, string) (string, error){f.s.Sweep, f.s.Recover, f.s.Cleanup} {
		cursor, err := job(ctx, "g", "")
		if err == nil || cursor != "" {
			t.Fatal("advanced failed page", cursor, err)
		}
	}
	if count(t, f, "g/messages") != 2 {
		t.Fatal("deletion survived rollback")
	}
}
