package groups

import (
	"context"
	"encoding/json"
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
	Items    []BulkItem        `json:"items"`
	Executed []BulkItem        `json:"executed"`
	Skipped  []BulkSkippedItem `json:"skipped"`
}

type BulkSkippedItem struct {
	Target     string `json:"target"`
	ReasonCode string `json:"reason_code"`
	Reason     string `json:"reason"`
}

func validBatchCommand(cmd Command) bool {
	if !validText(cmd.Text, 8192) || !validText(cmd.Message, 128) || !validText(cmd.Receiver, 128) || cmd.Seconds < 0 || cmd.Seconds > 31536000 {
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

type batchInputRecord struct {
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func decodeBatchDigest(raw json.RawMessage) (string, error) {
	var rec batchInputRecord
	if err := json.Unmarshal(raw, &rec); err == nil && rec.Digest != "" {
		return rec.Digest, nil
	}
	var legacy string
	if err := json.Unmarshal(raw, &legacy); err == nil && legacy != "" {
		return legacy, nil
	}
	return "", ErrInvalid
}

func (s *Service) Batch(ctx context.Context, p account.Principal, g, key string, in Bulk) (BulkResult, error) {
	out := newBulkResult()
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if len(in.Targets) < 1 || len(in.Targets) > 100 || !valid(g, 128) || !validKey(key) || !validBatchCommand(in.Command) {
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
		return s.previewBatch(ctx, p, g, in)
	}
	err := s.runScoped(ctx, []string{"group:" + g, "batch:" + p.Key() + ":" + g + ":" + key}, func(tx Tx) error {
		if _, err := s.batchConfig(ctx, tx, p, g, in.Command.Kind); err != nil {
			return err
		}
		var rawPrior json.RawMessage
		id := hash([]string{p.Key(), g, key})
		e := tx.Get("batch_inputs", id, &rawPrior)
		digest := hash(in)
		if e == nil {
			prior, err := decodeBatchDigest(rawPrior)
			if err != nil {
				return err
			}
			if prior != digest {
				return NewErrorDetail(ErrConflict, "idempotency_mismatch", "", 0)
			}
			return nil
		}
		if !errors.Is(e, ErrNotFound) {
			return e
		}
		return tx.Put("batch_inputs", id, batchInputRecord{Digest: digest, CreatedAt: s.now()})
	})
	if err != nil {
		return out, err
	}
	// Per-target receipts preserve independent outcomes. A preview performs no writes.
	for _, target := range in.Targets {
		cmd := in.Command
		cmd.Target = target
		outcome, err := s.executeBatchTarget(ctx, p, g, batchChildKey(g, key, target), cmd)
		if err != nil {
			return BulkResult{}, err
		}
		out.add(target, outcome)
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
	if len(in.Groups) < 1 || len(in.Groups) > 100 || !valid(in.Target, 2048) || !validKey(key) || strings.TrimSpace(in.Reason) == "" || !validText(in.Reason, 2048) || in.Seconds < 0 || in.Seconds > 31536000 {
		return out, ErrInvalid
	}
	scopes := []string{"receipt:" + p.Key() + ":" + key}
	for _, id := range in.Groups {
		scopes = append(scopes, "group:"+id)
	}
	e := s.runScoped(ctx, scopes, func(tx Tx) error {
		seen := map[string]bool{}
		for _, id := range in.Groups {
			if !valid(id, 128) || seen[id] {
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
		groups := slices.Clone(in.Groups)
		slices.Sort(groups)
		object := hash(groups)
		ok, e := replay(tx, p.Key(), "blacklists", object, key, in, &out)
		if e != nil || ok {
			return e
		}
		for _, id := range in.Groups {
			if in.Remove {
				e = tx.Delete(bucket(id, "blacklist"), in.Target)
			} else {
				now := s.now()
				created := now
				var prior Blacklist
				if err := tx.Get(bucket(id, "blacklist"), in.Target, &prior); err == nil && prior.Created != nil && !prior.Created.IsZero() {
					created = *prior.Created
				}
				e = tx.Put(bucket(id, "blacklist"), in.Target, Blacklist{ID: in.Target, Reason: in.Reason, Until: now.Add(time.Duration(in.Seconds) * time.Second), Permanent: in.Seconds == 0, Created: &created, Updated: &now})
			}
			if e != nil {
				return e
			}
			result := "blocked"
			if in.Remove {
				result = "unblocked"
			}
			if e = s.audit(tx, p, id, "blacklist", in.Target, result); e != nil {
				return e
			}
		}
		return s.saveReceipt(tx, p.Key(), "blacklists", object, key, in, out)
	})
	return out, e
}
func (s *Service) Progress(ctx context.Context, p account.Principal, g string) (Member, error) {
	var out Member
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !valid(g, 128) {
		return out, ErrInvalid
	}
	e := s.runRead(ctx, func(tx ReadTx) error { return tx.Get(bucket(g, "members"), p.Key(), &out) })
	out.ID = out.Owner
	return out, e
}
func (s *Service) manageRecord(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command) (Result, error) {
	out := Result{State: "completed"}
	if cmd.Kind == "watch" {
		if !valid(cmd.Target, 2048) {
			return out, ErrInvalid
		}
		now := s.now()
		created := now
		var prior Watch
		if e := tx.Get(bucket(c.ID, "watch"), cmd.Target, &prior); e == nil && prior.Created != nil && !prior.Created.IsZero() {
			created = *prior.Created
		}
		if e := tx.Put(bucket(c.ID, "watch"), cmd.Target, map[string]any{"enabled": cmd.Enabled, "reason": cmd.Text, "created": created, "updated": now}); e != nil {
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
		report["resolved_by"] = Actor(p)
		report["updated"] = s.now()
		if e := tx.Put(bucket(c.ID, "reports"), cmd.Target, report); e != nil {
			return out, e
		}
		return out, s.audit(tx, p, c.ID, "feedback", cmd.Target, "resolved")
	}
	if report["state"] != "suggested" {
		return out, ErrConflict
	}
	if c.RetentionSeconds == 0 {
		return out, ErrNotFound
	}
	raw, ok := report["suggestions"].([]any)
	if !ok {
		return out, ErrInvalid
	}
	executed := map[string]bool{}
	for _, suggestion := range raw {
		a, ok := suggestion.(string)
		if !ok || executed[a] || !slices.Contains(c.AutoActions, a) {
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
		command := Command{Kind: a, Target: m.Owner, Message: m.ID, Text: "ai_report:" + cmd.Target}
		if a == "mute" {
			command.Seconds = c.MuteSeconds
		}
		if _, e := s.command(ctx, tx, p, c, command, cap); e != nil {
			return out, e
		}
		executed[a] = true
	}
	if len(executed) == 0 {
		return out, nil
	}
	report["state"] = "pending_execution"
	if len(executed) == 1 && executed["blacklist"] {
		report["state"] = "resolved"
	}
	report["updated"] = s.now()
	return out, tx.Put(bucket(c.ID, "reports"), cmd.Target, report)
}

func batchChildKey(g, key, target string) string {
	if len(key) <= 100 {
		return key + "-" + hash(target)[:20]
	}
	return hash([]string{"batch", g, key, target})
}
