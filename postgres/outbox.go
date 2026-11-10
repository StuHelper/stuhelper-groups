package postgres

import (
	"context"
	"encoding/json"

	g "github.com/StuHelper/stuhelper-groups"
)

const pendingOutboxQuery = `SELECT envelope FROM groups.outbox
 WHERE sent_at IS NULL AND NOT (subject = ANY($2::text[]))
 ORDER BY subject COLLATE "C", (envelope->>'revision')::bigint, id COLLATE "C" LIMIT $1`

var _ g.OutboxReader = (*Repository)(nil)

// PendingOutbox is a non-claiming read. Retry/held subjects are owned by the
// consuming relay and must be passed explicitly; no provider clock is involved.
func (r *Repository) PendingOutbox(ctx context.Context, limit int, excludeSubjects []string) ([]g.Event, error) {
	if limit < 1 || limit > 100 || len(excludeSubjects) > 1000 {
		return nil, g.ErrInvalid
	}
	if excludeSubjects == nil {
		excludeSubjects = []string{}
	}
	rows, e := r.Pool.Query(ctx, pendingOutboxQuery, limit, excludeSubjects)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []g.Event{}
	for rows.Next() {
		var data []byte
		var event g.Event
		if e = rows.Scan(&data); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(data, &event); e != nil {
			return nil, e
		}
		out = append(out, event)
	}
	return out, rows.Err()
}
func (r *Repository) ConfirmOutbox(ctx context.Context, id string) error {
	tag, e := r.Pool.Exec(ctx, `UPDATE groups.outbox SET sent_at=COALESCE(sent_at,$2) WHERE id=$1`, id, r.now())
	if e != nil {
		return e
	}
	if tag.RowsAffected() == 0 {
		return g.ErrNotFound
	}
	return nil
}

// Standalone Relay keeps its own retry schedule in the same repository. Backend
// uses its shared relay schedule instead and calls only the public reader port.
func (r *Repository) deferredSubjects(ctx context.Context) ([]string, error) {
	rows, e := r.Pool.Query(ctx, `SELECT DISTINCT subject FROM groups.outbox WHERE sent_at IS NULL AND (held OR next_attempt>$1) ORDER BY subject LIMIT 1001`, r.now())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var subject string
		if e = rows.Scan(&subject); e != nil {
			return nil, e
		}
		out = append(out, subject)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	if len(out) > 1000 {
		return nil, g.ErrUnavailable
	}
	return out, nil
}
