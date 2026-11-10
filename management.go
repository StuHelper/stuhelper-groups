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

func (s *Service) Batch(ctx context.Context, p account.Principal, g, key string, in Bulk) (BulkResult, error) {
	out := BulkResult{Items: []BulkItem{}}
	if len(in.Targets) < 1 || len(in.Targets) > 100 || !valid(key, 100) || !slices.Contains([]string{"warn", "mute", "unmute", "kick", "recheck", "extend", "exempt", "card"}, in.Command.Kind) {
		return out, ErrInvalid
	}
	seen := map[string]bool{}
	for _, target := range in.Targets {
		if !valid(target, 2048) || seen[target] {
			return out, ErrInvalid
		}
		seen[target] = true
	}
	if !in.Preview {
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
	}
	// Per-target receipts preserve independent outcomes. A preview performs no writes.
	for _, target := range in.Targets {
		cmd := in.Command
		cmd.Target = target
		var r Result
		var e error
		if in.Preview {
			e = s.run(ctx, func(tx Tx) error {
				var c GroupConfig
				if e := tx.Get("config", g, &c); e != nil {
					return e
				}
				return s.auth(ctx, p, "groups.manage", c)
			})
			r = Result{State: "preview", Action: cmd.Kind, Detail: "target:" + target}
		} else {
			r, e = s.Command(ctx, p, g, key+"-"+hash(target)[:20], cmd)
		}
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
