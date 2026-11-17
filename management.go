package groups

import (
	"context"
	"errors"
	"github.com/StuHelper/stuhelper-platform/account"
	"slices"
	"strings"
	"time"
)

type Bulk struct {
	Targets []string `json:"targets"`
	Command Command  `json:"command"`
	Preview bool     `json:"preview"`
}
type BulkItem struct {
	Target string `json:"target"`
	Result Result `json:"result"`
}
type BulkResult struct {
	Items []BulkItem `json:"items"`
}

func validBatchCommand(cmd Command) bool {
	if len(cmd.Text) > 8192 || cmd.Seconds < 0 || cmd.Seconds > 31536000 {
		return false
	}
	switch cmd.Kind {
	case "warn", "exempt", "unexempt":
		return strings.TrimSpace(cmd.Text) != ""
	case "extend":
		return strings.TrimSpace(cmd.Text) != "" && cmd.Seconds > 0
	case "mute":
		return cmd.Seconds > 0 && cmd.Seconds <= 2592000
	case "unmute", "kick", "recheck", "card":
		return true
	default:
		return false
	}
}

func (s *Service) previewTarget(ctx context.Context, tx Tx, c GroupConfig, cmd Command, target string, pending int, cache map[factKey]Facts) (Result, error) {
	var m Member
	err := tx.Get(bucket(c.ID, "members"), target, &m)
	hasMember := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Result{}, err
	}
	switch cmd.Kind {
	case "extend":
		if !hasMember {
			return Result{State: "not_found", Action: "extend", Detail: "not_found"}, nil
		}
		if m.State == "removed" {
			return Result{State: "rejected", Action: "extend", Detail: "removed"}, nil
		}
		base := s.now()
		if m.Deadline.After(base) {
			base = m.Deadline
		}
		nextDeadline := base.Add(time.Duration(cmd.Seconds) * time.Second)
		return Result{State: "would_apply", Action: "extend", Value: cmd.Seconds, Detail: m.State + "->extended@" + nextDeadline.Format(time.RFC3339)}, nil
	case "exempt":
		if !hasMember {
			return Result{State: "not_found", Action: "exempt", Detail: "not_found"}, nil
		}
		if m.State == "removed" {
			return Result{State: "rejected", Action: "exempt", Detail: "removed"}, nil
		}
		if m.Exempt && m.State == "exempt" {
			return Result{State: "noop", Action: "exempt", Detail: "exempt->exempt"}, nil
		}
		return Result{State: "would_apply", Action: "exempt", Detail: m.State + "->exempt"}, nil
	case "unexempt":
		if !hasMember {
			return Result{State: "not_found", Action: "unexempt", Detail: "not_found"}, nil
		}
		if m.State == "removed" {
			return Result{State: "rejected", Action: "unexempt", Detail: "removed"}, nil
		}
		candidate := m
		candidate.Exempt = false
		assessment, e := s.assessEligibility(ctx, tx, c, candidate, cache)
		if e != nil {
			return Result{}, e
		}
		nextState := "grace"
		switch {
		case assessment.state == "eligible":
			nextState = "restored"
		case !m.Joined && c.Mode == "before":
			nextState = "awaiting_verification"
		}
		if !m.Exempt {
			return Result{State: "noop", Action: "unexempt", Detail: m.State + "->" + m.State}, nil
		}
		return Result{State: "would_apply", Action: "unexempt", Detail: m.State + "->" + nextState}, nil
	case "recheck":
		if !hasMember {
			return Result{State: "not_found", Action: "recheck", Detail: "not_found"}, nil
		}
		if m.State == "removed" {
			return Result{State: "noop", Action: "recheck", Detail: "removed->removed"}, nil
		}
		if m.Exempt {
			return Result{State: "noop", Action: "recheck", Detail: m.State + "->exempt"}, nil
		}
		assessment, e := s.assessEligibility(ctx, tx, c, m, cache)
		if e != nil {
			return Result{}, e
		}
		nextState := m.State
		switch {
		case assessment.state == "eligible":
			nextState = "restored"
		case c.Mode == "before" && m.State == "awaiting_verification":
			nextState = "awaiting_verification"
		case !m.Deadline.IsZero() && !s.now().Before(m.Deadline) && m.State != "removal_pending":
			if !c.BotEnabled {
				return Result{State: "rejected", Action: "recheck", Detail: ErrDisabled.Error()}, nil
			}
			if pending >= 1000 {
				return Result{State: "rejected", Action: "recheck", Detail: ErrCooldown.Error()}, nil
			}
			nextState = "removal_pending"
		case m.Deadline.IsZero():
			nextState = "grace"
		}
		state := "would_apply"
		if nextState == m.State {
			state = "noop"
		}
		return Result{State: state, Action: "recheck", Detail: m.State + "->" + nextState}, nil
	case "card":
		r, e := s.evaluateCard(ctx, c, target, cmd.Text, cache)
		if errors.Is(e, ErrDisabled) {
			return Result{State: "rejected", Action: "card", Detail: ErrDisabled.Error()}, nil
		}
		if e != nil {
			return Result{}, e
		}
		if r.State == "noncompliant" && (c.RemindCard || c.AutoCard) {
			if !c.BotEnabled {
				return Result{State: "rejected", Action: "card", Detail: ErrDisabled.Error()}, nil
			}
			if pending >= 1000 {
				return Result{State: "rejected", Action: "card", Detail: ErrCooldown.Error()}, nil
			}
		}
		return r, nil
	default:
		if !c.BotEnabled {
			return Result{State: "rejected", Action: cmd.Kind, Detail: ErrDisabled.Error()}, nil
		}
		if pending >= 1000 {
			return Result{State: "rejected", Action: cmd.Kind, Detail: ErrCooldown.Error()}, nil
		}
		if !hasMember {
			return Result{State: "not_found", Action: cmd.Kind, Detail: "not_found"}, nil
		}
		if !m.Joined || m.State == "removed" {
			return Result{State: "not_joined", Action: cmd.Kind, Detail: m.State}, nil
		}
		fromState := m.State
		if m.Exempt {
			fromState = "exempt"
		}
		return Result{State: "would_queue", Action: cmd.Kind, Value: cmd.Seconds, Detail: fromState + "->pending"}, nil
	}
}

func (s *Service) Batch(ctx context.Context, p account.Principal, g, key string, in Bulk) (BulkResult, error) {
	out := BulkResult{Items: []BulkItem{}}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !valid(g, 128) || len(in.Targets) < 1 || len(in.Targets) > 100 || !valid(key, 128) || !validBatchCommand(in.Command) {
		return out, ErrInvalid
	}
	seen := map[string]bool{}
	for _, target := range in.Targets {
		if !valid(target, 2048) || seen[target] {
			return out, ErrInvalid
		}
		seen[target] = true
	}
	if in.Preview {
		err := s.run(ctx, func(tx Tx) error {
			var c GroupConfig
			if e := tx.Get("config", g, &c); e != nil {
				return e
			}
			if e := s.auth(ctx, p, "groups.manage", c); e != nil {
				return e
			}
			pending, e := tx.PendingActions(c.ID)
			if e != nil {
				return e
			}
			cache := map[factKey]Facts{}
			for _, target := range in.Targets {
				r, e := s.previewTarget(ctx, tx, c, in.Command, target, pending, cache)
				if e != nil {
					return e
				}
				out.Items = append(out.Items, BulkItem{Target: target, Result: r})
			}
			return nil
		})
		if err != nil {
			return BulkResult{Items: []BulkItem{}}, err
		}
		return out, nil
	}
	err := s.run(ctx, func(tx Tx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		if e := s.auth(ctx, p, "groups.manage", c); e != nil {
			return e
		}
		var prior string
		id := hash([]string{p.Key(), g, key})
		e := tx.Get("batch_inputs", id, &prior)
		digest := hash(in)
		if e == nil {
			if prior != digest {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(e, ErrNotFound) {
			return e
		}
		return tx.Put("batch_inputs", id, digest)
	})
	if err != nil {
		return out, err
	}
	// Per-target receipts preserve independent outcomes. A preview performs no writes.
	for _, target := range in.Targets {
		cmd := in.Command
		cmd.Target = target
		r, e := s.Command(ctx, p, g, hash([]string{"batch", g, key, target}), cmd)
		if e != nil {
			r = Result{State: "rejected", Detail: ErrUnavailable.Error()}
			for _, known := range []error{ErrInvalid, ErrForbidden, ErrUnauthenticated, ErrNotFound, ErrConflict, ErrDisabled, ErrUnprocessable, ErrCooldown} {
				if errors.Is(e, known) {
					r.Detail = known.Error()
					break
				}
			}
		}
		out.Items = append(out.Items, BulkItem{target, r})
	}
	return out, nil
}

type BlacklistInput struct {
	Groups  []string `json:"groups"`
	Target  string   `json:"target"`
	Reason  string   `json:"reason"`
	Seconds int64    `json:"seconds"`
	Remove  bool     `json:"remove"`
}

func (s *Service) Blacklists(ctx context.Context, p account.Principal, key string, in BlacklistInput) (Result, error) {
	out := Result{State: "completed"}
	if len(in.Groups) < 1 || len(in.Groups) > 100 || !valid(in.Target, 2048) || !valid(key, 128) || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 2048 || in.Seconds < 0 || in.Seconds > 31536000 {
		return out, ErrInvalid
	}
	e := s.run(ctx, func(tx Tx) error {
		seen := map[string]bool{}
		for _, id := range in.Groups {
			if seen[id] {
				return ErrInvalid
			}
			seen[id] = true
			c := GroupConfig{GroupSettings: GroupSettings{ID: "*", School: "*"}}
			if id == "*" {
				if s.PlatformIdentity == nil {
					return ErrUnavailable
				}
				isPlatform, err := s.PlatformIdentity.PlatformAdmin(ctx, p)
				if err != nil {
					return err
				}
				if !isPlatform {
					return ErrForbidden
				}
			}
			if id != "*" {
				if e := tx.Get("config", id, &c); e != nil {
					return e
				}
			}
			if e := s.auth(ctx, p, "groups.manage", c); e != nil {
				return e
			}
		}
		ok, e := replay(tx, p.Key(), key, in, &out)
		if e != nil || ok {
			return e
		}
		for _, id := range in.Groups {
			if in.Remove {
				e = tx.Delete(bucket(id, "blacklist"), in.Target)
			} else {
				e = tx.Put(bucket(id, "blacklist"), in.Target, Blacklist{ID: in.Target, Reason: in.Reason, Until: s.now().Add(time.Duration(in.Seconds) * time.Second), Permanent: in.Seconds == 0})
			}
			if e != nil {
				return e
			}
			if e = s.audit(tx, p, id, "blacklist", in.Target, "updated"); e != nil {
				return e
			}
		}
		return save(tx, p.Key(), key, in, out)
	})
	return out, e
}
func (s *Service) Progress(ctx context.Context, p account.Principal, g string) (Member, error) {
	var out Member
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	e := s.run(ctx, func(tx Tx) error { return tx.Get(bucket(g, "members"), p.Key(), &out) })
	out.ID = out.Owner
	return out, e
}
func (s *Service) manageRecord(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command) (Result, error) {
	out := Result{State: "completed"}
	if cmd.Kind == "watch" {
		if !valid(cmd.Target, 2048) {
			return out, ErrInvalid
		}
		if e := tx.Put(bucket(c.ID, "watch"), cmd.Target, map[string]any{"enabled": cmd.Enabled, "reason": cmd.Text}); e != nil {
			return out, e
		}
		return out, s.audit(tx, p, c.ID, "watch", cmd.Target, "updated")
	}
	var report map[string]any
	if e := tx.Get(bucket(c.ID, "reports"), cmd.Target, &report); e != nil {
		return out, e
	}
	if cmd.Kind == "feedback" {
		if cmd.Text == "" {
			return out, ErrInvalid
		}
		report["feedback"] = cmd.Text
		report["state"] = "resolved"
		if e := tx.Put(bucket(c.ID, "reports"), cmd.Target, report); e != nil {
			return out, e
		}
		return out, s.audit(tx, p, c.ID, "feedback", cmd.Target, "resolved")
	}
	raw, ok := report["suggestions"].([]any)
	if !ok {
		return out, ErrInvalid
	}
	for _, suggestion := range raw {
		a, ok := suggestion.(string)
		if !ok || !slices.Contains(c.AutoActions, a) {
			continue
		}
		cap := capability(a)
		if e := s.auth(ctx, p, cap, c); e != nil {
			return out, e
		}
		if !slices.Contains([]string{"warn", "mute", "recall", "kick", "blacklist"}, a) {
			continue
		}
		var m Message
		message, _ := report["message"].(string)
		if e := tx.Get(bucket(c.ID, "messages"), message, &m); e != nil {
			return out, e
		}
		if !s.now().Before(m.Expires) {
			return out, ErrNotFound
		}
		command := Command{Kind: a, Target: m.Owner, Message: m.ID, Text: "ai_report:" + cmd.Target, Seconds: c.MuteSeconds}
		if _, e := s.command(ctx, tx, p, c, command, cap); e != nil {
			return out, e
		}
	}
	report["state"] = "pending_execution"
	return out, tx.Put(bucket(c.ID, "reports"), cmd.Target, report)
}
