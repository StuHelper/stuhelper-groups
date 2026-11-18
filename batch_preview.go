package groups

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/StuHelper/stuhelper-platform/account"
	"slices"
	"time"
)

// Simulate one target's writes on a read-only database snapshot. The batch
// carries forward queue usage only after the entire target succeeds.
type previewTx struct {
	ReadTx
	rows        map[[2]string]json.RawMessage
	group       string
	pending     int
	factsFailed bool
}

func (t *previewTx) Get(bucket, id string, out any) error {
	raw, ok := t.rows[[2]string{bucket, id}]
	if !ok {
		return t.ReadTx.Get(bucket, id, out)
	}
	if raw == nil {
		return ErrNotFound
	}
	return json.Unmarshal(raw, out)
}
func (t *previewTx) Put(bucket, id string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if bucket == t.group+"/actions" {
		t.pending++
	}
	t.rows[[2]string{bucket, id}] = raw
	return nil
}
func (t *previewTx) Delete(bucket, id string) error { t.rows[[2]string{bucket, id}] = nil; return nil }
func (t *previewTx) PendingActions(group string) (int, error) {
	if group != t.group {
		return 0, ErrInvalid
	}
	return t.pending, nil
}
func (*previewTx) DeleteSentOutboxBefore(time.Time, int) (int, error) { return 0, ErrInvalid }
func (*previewTx) Emit(Event) error                                   { return nil }
func (*previewTx) Accept(Event) (bool, error)                         { return false, ErrInvalid }

func (s *Service) previewBatch(ctx context.Context, p account.Principal, id string, in Bulk) (BulkResult, error) {
	out := BulkResult{Items: []BulkItem{}}
	err := s.runRead(ctx, func(tx ReadTx) error {
		var c GroupConfig
		if err := tx.Get("config", id, &c); err != nil {
			return err
		}
		if err := s.auth(ctx, p, "groups.manage", c); err != nil {
			return err
		}
		if cap := capability(in.Command.Kind); cap != "groups.manage" {
			if err := s.auth(ctx, p, cap, c); err != nil {
				return err
			}
		}
		pending, err := tx.PendingActions(id)
		if err != nil {
			return err
		}
		for _, target := range in.Targets {
			trial := &previewTx{ReadTx: tx, rows: make(map[[2]string]json.RawMessage), group: id, pending: pending}
			cmd := in.Command
			cmd.Target = target
			result, err := s.previewCommand(ctx, trial, p, c, cmd)
			if err != nil {
				if !trial.factsFailed && !errors.Is(err, ErrCooldown) && !errors.Is(err, ErrDisabled) && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
					return err
				}
				result = Result{State: "rejected", Action: cmd.Kind, Detail: err.Error()}
			} else {
				pending = trial.pending
			}
			out.Items = append(out.Items, BulkItem{target, result})
		}
		return nil
	})
	if err != nil {
		return BulkResult{Items: []BulkItem{}}, err
	}
	return out, nil
}

func (s *Service) previewCommand(ctx context.Context, tx *previewTx, p account.Principal, c GroupConfig, cmd Command) (Result, error) {
	var m Member
	err := tx.Get(bucket(c.ID, "members"), cmd.Target, &m)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Result{}, err
	}
	missing := errors.Is(err, ErrNotFound)
	if missing && cmd.Kind != "card" {
		return Result{Action: cmd.Kind, State: "not_found", Detail: "not_found"}, nil
	}
	botAction := slices.Contains([]string{"warn", "mute", "unmute", "kick"}, cmd.Kind)
	if botAction && (!m.Joined || m.State == "removed") {
		return Result{Action: cmd.Kind, State: "not_joined", Detail: m.State}, nil
	}
	before := m.State
	if m.Exempt {
		before = "exempt"
	}
	result, err := s.command(ctx, tx, p, c, cmd, capability(cmd.Kind))
	if err != nil {
		return Result{}, err
	}
	if botAction {
		return Result{Action: cmd.Kind, State: "would_queue", Value: cmd.Seconds, Detail: before + "->pending"}, nil
	}
	if cmd.Kind == "card" {
		if result.State == "pending" {
			result.State = "would_queue"
			result.ID = ""
		}
		return result, nil
	}
	if err := tx.Get(bucket(c.ID, "members"), cmd.Target, &m); err != nil {
		return Result{}, err
	}
	out := Result{Action: cmd.Kind, State: "would_apply", Detail: before + "->" + m.State}
	if before == m.State {
		out.State = "noop"
	}
	if cmd.Kind == "extend" {
		out.State = "would_apply"
		out.Value = cmd.Seconds
		out.Detail += "@" + m.Deadline.Format(time.RFC3339)
	}
	return out, nil
}
