// Package postgres owns all groups SQL. Use a dedicated groups database role.
package postgres

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

//go:embed schema.sql
var Schema string

type Repository struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
}

func (r *Repository) Migrate(ctx context.Context) error { _, e := r.Pool.Exec(ctx, Schema); return e }
func (r *Repository) Ready(ctx context.Context) error   { return r.Pool.Ping(ctx) }

type transaction struct {
	ctx context.Context
	tx  pgx.Tx
}

func (r *Repository) Transaction(ctx context.Context, fn func(g.Tx) error) error {
	tx, e := r.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // Module-local serialization prevents lost updates across workflows.
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7420719)"); e != nil {
		return e
	}
	if e = fn(&transaction{ctx, tx}); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (t *transaction) Get(bucket, id string, v any) error {
	var b []byte
	e := t.tx.QueryRow(t.ctx, "SELECT data FROM groups.records WHERE bucket=$1 AND id=$2", bucket, id).Scan(&b)
	if errors.Is(e, pgx.ErrNoRows) {
		return g.ErrNotFound
	}
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func (t *transaction) Put(bucket, id string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = t.tx.Exec(t.ctx, "INSERT INTO groups.records(bucket,id,data) VALUES($1,$2,$3) ON CONFLICT(bucket,id) DO UPDATE SET data=EXCLUDED.data", bucket, id, b)
	return e
}
func (t *transaction) Delete(bucket, id string) error {
	_, e := t.tx.Exec(t.ctx, "DELETE FROM groups.records WHERE bucket=$1 AND id=$2", bucket, id)
	return e
}
func (t *transaction) List(bucket, cursor string, limit int) ([]g.Row, error) {
	if limit < 1 || limit > 100 {
		return nil, g.ErrInvalid
	}
	rows, e := t.tx.Query(t.ctx, "SELECT id,data FROM groups.records WHERE bucket=$1 AND id>$2 ORDER BY id LIMIT $3", bucket, cursor, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []g.Row{}
	for rows.Next() {
		var r g.Row
		if e = rows.Scan(&r.ID, &r.Data); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (t *transaction) Emit(event g.Event) error {
	var revision int64
	e := t.tx.QueryRow(t.ctx, "INSERT INTO groups.revisions(source,subject,revision) VALUES('groups',$1,1) ON CONFLICT(source,subject) DO UPDATE SET revision=groups.revisions.revision+1 RETURNING revision", event.Subject).Scan(&revision)
	if e != nil {
		return e
	}
	event.Revision = revision
	b, e := json.Marshal(event)
	if e != nil {
		return e
	}
	_, e = t.tx.Exec(t.ctx, "INSERT INTO groups.outbox(id,source,subject,envelope,created_at,next_attempt) VALUES($1,$2,$3,$4,$5,$5)", event.ID, event.Source, event.Subject, b, event.Time)
	if e != nil {
		return e
	}
	_, e = t.tx.Exec(t.ctx, "SELECT pg_notify('groups_outbox','')")
	return e
}
func (t *transaction) Accept(event g.Event) (bool, error) {
	if event.SpecVersion != "1.0" || event.ID == "" || event.Source == "" || event.Subject == "" || event.Revision < 1 || event.Time.IsZero() {
		return false, g.ErrInvalid
	}
	b, e := json.Marshal(event)
	if e != nil {
		return false, e
	}
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	var old string
	e = t.tx.QueryRow(t.ctx, "SELECT digest FROM groups.inbox WHERE source=$1 AND id=$2", event.Source, event.ID).Scan(&old)
	if e == nil {
		if old != digest {
			return false, g.ErrConflict
		}
		return false, nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return false, e
	}
	_, e = t.tx.Exec(t.ctx, "INSERT INTO groups.inbox(source,id,digest) VALUES($1,$2,$3)", event.Source, event.ID, digest)
	if e != nil {
		return false, e
	}
	tag, e := t.tx.Exec(t.ctx, "INSERT INTO groups.revisions(source,subject,revision) VALUES($1,$2,$3) ON CONFLICT(source,subject) DO UPDATE SET revision=EXCLUDED.revision WHERE groups.revisions.revision<EXCLUDED.revision", event.Source, event.Subject, event.Revision)
	return tag.RowsAffected() == 1, e
}

// Pending returns only each stream's earliest unsent event, preserving order.
func (r *Repository) Pending(ctx context.Context, now time.Time) ([]g.Event, error) {
	rows, e := r.Pool.Query(ctx, `SELECT envelope FROM groups.outbox o WHERE sent_at IS NULL AND NOT held AND next_attempt<=$1 AND NOT EXISTS (SELECT 1 FROM groups.outbox p WHERE p.sent_at IS NULL AND p.source=o.source AND p.subject=o.subject AND p.sequence<o.sequence) ORDER BY sequence LIMIT 100`, now)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []g.Event{}
	for rows.Next() {
		var b []byte
		var v g.Event
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (r *Repository) Delivered(ctx context.Context, id string, now time.Time) error {
	_, e := r.Pool.Exec(ctx, "UPDATE groups.outbox SET sent_at=$2 WHERE id=$1 AND sent_at IS NULL", id, now)
	return e
}
func (r *Repository) Failed(ctx context.Context, id string, now time.Time) error {
	_, e := r.Pool.Exec(ctx, `UPDATE groups.outbox SET attempts=attempts+1,next_attempt=$2::timestamptz+make_interval(secs=>LEAST(3600,5*power(2,LEAST(attempts,10)))::double precision),held=($2::timestamptz-created_at>=interval '24 hours') WHERE id=$1 AND sent_at IS NULL`, id, now)
	return e
}

// Retry is a host-only administrative boundary; host must authorize and audit.
func (r *Repository) Retry(ctx context.Context, id string, now time.Time) error {
	_, e := r.Pool.Exec(ctx, "UPDATE groups.outbox SET held=false,attempts=0,created_at=$2,next_attempt=$2 WHERE id=$1 AND sent_at IS NULL", id, now)
	return e
}

func (t *transaction) PendingActions(group string) (int, error) {
	var n int
	e := t.tx.QueryRow(t.ctx, "SELECT count(*) FROM (SELECT 1 FROM groups.records WHERE bucket=$1 AND data->>'state' IN ('pending','executing','unknown') LIMIT 1000) q", group+"/actions").Scan(&n)
	return n, e
}
