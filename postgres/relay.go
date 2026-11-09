package postgres

import (
	"context"
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"time"
)

// Relay owns one LISTEN connection. The host restarts Relay after connection
// failure, with bounded backoff. Startup and reconnect immediately drain outbox.
// deliver must persist consumer inbox before returning nil.
func (r *Repository) Relay(ctx context.Context, deliver func(context.Context, g.Event) error) error {
	conn, e := r.Pool.Acquire(ctx)
	if e != nil {
		return e
	}
	defer conn.Release()
	if _, e = conn.Exec(ctx, "LISTEN groups_outbox"); e != nil {
		return e
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_, _ = conn.Exec(c, "UNLISTEN groups_outbox")
	}()
	for {
		if e = ctx.Err(); e != nil {
			return e
		}
		excluded, e := r.deferredSubjects(ctx)
		if e != nil {
			return e
		}
		pending, e := r.PendingOutbox(ctx, 100, excluded)
		if e != nil {
			return e
		}
		failed := map[string]bool{}
		for _, event := range pending {
			if failed[event.Subject] {
				continue
			}
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := deliver(call, event)
			cancel()
			if err == nil {
				e = r.ConfirmOutbox(ctx, event.ID)
			} else {
				failed[event.Subject] = true
				e = r.Failed(ctx, event.ID, r.now())
			}
			if e != nil {
				return e
			}
		}
		if len(pending) > 0 {
			continue
		}
		delay := 60 * time.Second
		var due *time.Time
		e = r.Pool.QueryRow(ctx, `SELECT min(next_attempt) FROM groups.outbox o WHERE sent_at IS NULL AND NOT held AND NOT EXISTS(SELECT 1 FROM groups.outbox p WHERE p.sent_at IS NULL AND p.source=o.source AND p.subject=o.subject AND p.sequence<o.sequence)`).Scan(&due)
		if e != nil {
			return e
		}
		if due != nil {
			d := due.Sub(r.now())
			if d < time.Millisecond {
				d = time.Millisecond
			}
			if d < delay {
				delay = d
			}
		}
		wait, cancel := context.WithTimeout(ctx, delay)
		_, e = conn.Conn().WaitForNotification(wait)
		cancel()
		if e != nil && !errors.Is(e, context.DeadlineExceeded) {
			return e
		}
	}
}

func (r *Repository) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
