package groups

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/StuHelper/stuhelper-platform/account"
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
	out := newBulkResult()
	err := s.runRead(ctx, func(tx ReadTx) error {
		c, err := s.batchConfig(ctx, tx, p, id, in.Command.Kind)
		if err != nil {
			return err
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
			outcome := batchOutcome{Result: result}
			if err != nil {
				var known bool
				outcome, known = batchRejection(cmd, err)
				if trial.factsFailed && errors.Is(err, ErrUnavailable) {
					outcome = batchOutcome{Result: Result{Action: cmd.Kind, State: "rejected", Detail: ErrUnavailable.Error()}, SkipCode: ErrUnavailable.Error()}
				} else if !known {
					return err
				}
			} else {
				pending = trial.pending
			}
			out.add(target, outcome)
		}
		return nil
	})
	if err != nil {
		return BulkResult{}, err
	}
	return out, nil
}

func (s *Service) previewCommand(ctx context.Context, tx *previewTx, p account.Principal, c GroupConfig, cmd Command) (Result, error) {
	m, result, err := s.batchCommand(ctx, tx, p, c, cmd)
	if err != nil || result.State == "not_found" || result.State == "not_joined" {
		return result, err
	}
	before := m.State
	if m.Exempt {
		before = "exempt"
	}
	if batchRequiresJoined(cmd.Kind) {
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
	if result.State == "noop" || result.State == "removed" {
		out.State = result.State
	}
	if cmd.Kind == "extend" {
		out.State = "would_apply"
		out.Value = cmd.Seconds
		out.Detail += "@" + m.Deadline.Format(time.RFC3339)
	}
	return out, nil
}
