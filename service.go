package groups

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/StuHelper/stuhelper-platform/account"
	"github.com/StuHelper/stuhelper-platform/admin"
	"github.com/google/uuid"
)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func valid(v string, n int) bool {
	return strings.TrimSpace(v) != "" && len(v) <= n && !strings.ContainsFunc(v, unicode.IsControl) && !strings.Contains(v, "/")
}
func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func bucket(g, k string) string { return g + "/" + k }
func (s *Service) run(ctx context.Context, fn func(Tx) error) error {
	if s.Repo == nil {
		return ErrUnavailable
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.Repo.Transaction(c, fn)
}
func (s *Service) auth(ctx context.Context, p account.Principal, c string, g GroupConfig) error {
	if !p.Valid() {
		return ErrUnauthenticated
	}
	if s.Auth == nil {
		return ErrUnavailable
	}
	e := s.Auth.Evaluate(ctx, p, c, admin.Scope{School: g.School, Group: g.ID, Module: "groups"})
	if e != nil {
		if errors.Is(e, admin.ErrForbidden) {
			return ErrForbidden
		}
		if errors.Is(e, admin.ErrUnauthenticated) {
			return ErrUnauthenticated
		}
		return e
	}
	return nil
}
func (s *Service) event(tx Tx, g, kind string, data any) error {
	b, e := json.Marshal(data)
	if e != nil {
		return e
	}
	return tx.Emit(Event{SpecVersion: "1.0", ID: uuid.NewString(), Source: "groups", Type: "groups." + kind, Subject: g, Time: s.now(), Data: b})
}
func (s *Service) audit(tx Tx, p account.Principal, g, kind, target, result string) error {
	actor := p.Key()
	if actor == "" {
		actor = "system:groups"
	}
	return tx.Put(bucket(g, "audit"), uuid.NewString(), map[string]any{"actor": actor, "action": kind, "target": target, "result": result, "time": s.now()})
}

type receipt struct {
	Digest string
	Result json.RawMessage
}

func replay(tx Tx, owner, key string, input, out any) (bool, error) {
	var r receipt
	e := tx.Get("receipts", hash([]string{owner, key}), &r)
	if errors.Is(e, ErrNotFound) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if r.Digest != hash(input) {
		return false, NewErrorDetail(ErrConflict, "idempotency_mismatch", "", 0)
	}
	return true, json.Unmarshal(r.Result, out)
}
func save(tx Tx, owner, key string, input, out any) error {
	b, e := json.Marshal(out)
	if e != nil {
		return e
	}
	return tx.Put("receipts", hash([]string{owner, key}), receipt{hash(input), b})
}
func validateConfig(c GroupConfig) error {
	if c.ID == "me" || c.ID == "*" || !valid(c.ID, 128) {
		return NewErrorDetail(ErrInvalid, "invalid_field", "id", 0)
	}
	if c.School == "*" || !valid(c.School, 128) {
		return NewErrorDetail(ErrInvalid, "invalid_field", "school", 0)
	}
	if strings.TrimSpace(c.Name) == "" || utf8.RuneCountInString(c.Name) > 128 || strings.ContainsFunc(c.Name, unicode.IsControl) {
		return NewErrorDetail(ErrInvalid, "invalid_field", "name", 0)
	}
	if !slices.Contains([]string{"before", "muted", "password"}, c.Mode) {
		return NewErrorDetail(ErrInvalid, "invalid_field", "mode", 0)
	}
	if c.GraceSeconds <= 0 || c.GraceSeconds > 31536000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "grace_seconds", 0)
	}
	if c.ReminderSeconds < 0 || c.ReminderSeconds > 31536000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "reminder_seconds", 0)
	}
	if c.RetentionSeconds < 0 || c.RetentionSeconds > 31536000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "retention_seconds", 0)
	}
	if c.CooldownSeconds < 0 || c.CooldownSeconds > 31536000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "cooldown_seconds", 0)
	}
	if c.DiceSides < 0 || c.DiceSides == 1 || c.DiceSides > 1000000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "dice_sides", 0)
	}
	if c.MuteProbability < 0 || c.MuteProbability > 100 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "mute_probability", 0)
	}
	if c.MuteSeconds < 0 || c.MuteSeconds > 2592000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "mute_seconds", 0)
	}
	if c.Pity < 0 || c.Pity > 10000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "pity", 0)
	}
	if c.RepeatThreshold < 0 || c.RepeatThreshold > 1000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "repeat_threshold", 0)
	}
	if len(c.Keywords) > 100 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "keywords", 0)
	}
	if len(c.CardTemplate) > 1024 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "card_template", 0)
	}
	if len(c.Welcome) > 2048 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "welcome", 0)
	}
	if len(c.RulesTemplate) > 8192 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "rules_template", 0)
	}
	if c.FrequencySeconds < 0 || c.FrequencySeconds > 86400 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "frequency_seconds", 0)
	}
	if c.FrequencyLimit < 0 || c.FrequencyLimit > 10000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "frequency_limit", 0)
	}
	if c.WarningLimit < 0 || c.WarningLimit > 10000 {
		return NewErrorDetail(ErrInvalid, "invalid_field", "warning_limit", 0)
	}
	if c.MuteProbability > 0 && c.MuteSeconds == 0 {
		return NewErrorDetail(ErrInvalid, "missing_mute_seconds", "mute_seconds", 0)
	}
	if c.MuteProbability > 0 && c.CooldownSeconds == 0 {
		return NewErrorDetail(ErrInvalid, "missing_cooldown_seconds", "cooldown_seconds", 0)
	}
	if c.Mode == "password" && !c.PasswordSet {
		return NewErrorDetail(ErrInvalid, "missing_password", "password", 0)
	}
	for _, k := range c.Keywords {
		if strings.TrimSpace(k) == "" || len(k) > 256 {
			return NewErrorDetail(ErrInvalid, "invalid_field", "keywords", 0)
		}
	}
	if slices.Contains(c.AutoActions, "mute") && c.MuteSeconds == 0 {
		return NewErrorDetail(ErrInvalid, "missing_mute_seconds", "mute_seconds", 0)
	}
	for _, a := range c.AutoActions {
		if !slices.Contains([]string{"warn", "recall", "mute", "kick", "blacklist"}, a) {
			return NewErrorDetail(ErrInvalid, "invalid_field", "auto_actions", 0)
		}
	}
	return nil
}
func capability(kind string) string {
	switch kind {
	case "send", "forward":
		return "groups.messages.send"
	case "recall":
		return "groups.messages.recall"
	case "extend", "exempt", "warn", "mute", "unmute", "kick", "blacklist", "unblacklist":
		return "groups.manage"
	case "subscribe", "rule", "ai_execute":
		return "groups.configure"
	case "feedback", "watch":
		return "groups.manage"
	case "analyze", "preview", "card", "recheck":
		return "groups.read"
	}
	return ""
}
func (s *Service) queue(tx Tx, p account.Principal, c GroupConfig, cmd Command, cap string) (Result, error) {
	if !c.BotEnabled {
		return Result{}, NewErrorDetail(ErrDisabled, "bot_disabled", "bot_enabled", 0)
	}
	pending, e := tx.PendingActions(c.ID)
	if e != nil {
		return Result{}, e
	}
	if pending >= 1000 {
		return Result{}, NewErrorDetail(ErrCooldown, "action_queue_full", "", 0)
	}
	a := Action{ID: uuid.NewString(), Group: c.ID, Actor: Actor(p), Capability: cap, Command: cmd, State: "pending", Created: s.now()}
	if e := tx.Put(bucket(c.ID, "actions"), a.ID, a); e != nil {
		return Result{}, e
	}
	if e := s.audit(tx, p, c.ID, cmd.Kind, cmd.Target, "pending"); e != nil {
		return Result{}, e
	}
	if e := s.event(tx, c.ID, "action.requested", map[string]string{"id": a.ID}); e != nil {
		return Result{}, e
	}
	return Result{ID: a.ID, State: "pending", Action: cmd.Kind}, nil
}
func (s *Service) Command(ctx context.Context, p account.Principal, g, key string, cmd Command) (Result, error) {
	var out Result
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !valid(key, 128) || !valid(g, 128) || len(cmd.Text) > 8192 || cmd.Seconds < 0 || cmd.Seconds > 31536000 {
		return out, ErrInvalid
	}
	e := s.run(ctx, func(tx Tx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		cap := capability(cmd.Kind)
		if cap != "" {
			if e := s.auth(ctx, p, cap, c); e != nil {
				return e
			}
		} else {
			if !slices.Contains([]string{"apply", "password", "dice", "self_mute", "report"}, cmd.Kind) {
				return ErrInvalid
			}
			if cmd.Target != "" && cmd.Target != p.Key() {
				return ErrForbidden
			}
			cmd.Target = p.Key()
		}
		input := struct {
			Group   string
			Command Command
		}{g, cmd}
		ok, e := replay(tx, p.Key(), key, input, &out)
		if e != nil || ok {
			return e
		}
		out, e = s.command(ctx, tx, p, c, cmd, cap)
		if e != nil {
			return e
		}
		return save(tx, p.Key(), key, input, out)
	})
	return out, e
}
func (s *Service) command(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command, cap string) (Result, error) {
	out := Result{State: "completed"}
	switch cmd.Kind {
	case "apply", "recheck":
		return s.admission(ctx, tx, p, c, cmd.Target, cmd.Kind == "apply")
	case "extend", "exempt":
		var m Member
		if e := tx.Get(bucket(c.ID, "members"), cmd.Target, &m); e != nil {
			return out, e
		}
		if strings.TrimSpace(cmd.Text) == "" {
			return out, ErrInvalid
		}
		if cmd.Kind == "extend" {
			if cmd.Seconds == 0 {
				return out, ErrInvalid
			}
			m.Deadline = s.now().Add(time.Duration(cmd.Seconds) * time.Second)
			m.Extended = true
			m.State = "extended"
		} else {
			m.Exempt = true
			m.State = "exempt"
		}
		out.State = m.State
		if e := tx.Put(bucket(c.ID, "members"), cmd.Target, m); e != nil {
			return out, e
		}
		return out, s.audit(tx, p, c.ID, cmd.Kind, cmd.Target, m.State)
	case "password":
		if c.Mode != "password" {
			return out, NewErrorDetail(ErrDisabled, "password_mode_disabled", "mode", 0)
		}
		var stored configRecord
		if e := tx.Get("config", c.ID, &stored); e != nil {
			return out, e
		}
		if !checkPassword(cmd.Text, stored.PasswordDigest) {
			return out, ErrForbidden
		}
		var m Member
		if e := tx.Get(bucket(c.ID, "members"), cmd.Target, &m); e != nil {
			return out, e
		}
		m.Password = true
		if e := tx.Put(bucket(c.ID, "members"), cmd.Target, m); e != nil {
			return out, e
		}
		return s.admission(ctx, tx, p, c, cmd.Target, false)
	case "dice", "self_mute":
		return s.play(ctx, tx, p, c, cmd)
	case "blacklist":
		if strings.TrimSpace(cmd.Text) == "" {
			return out, ErrInvalid
		}
		if !valid(cmd.Target, 2048) {
			return out, ErrInvalid
		}
		if e := tx.Put(bucket(c.ID, "blacklist"), cmd.Target, Blacklist{ID: cmd.Target, Reason: cmd.Text, Until: s.now().Add(time.Duration(cmd.Seconds) * time.Second), Permanent: cmd.Seconds == 0}); e != nil {
			return out, e
		}
		return out, s.audit(tx, p, c.ID, cmd.Kind, cmd.Target, "blocked")
	case "unblacklist":
		if e := tx.Delete(bucket(c.ID, "blacklist"), cmd.Target); e != nil {
			return out, e
		}
		return out, s.audit(tx, p, c.ID, cmd.Kind, cmd.Target, "unblocked")
	case "report":
		if cmd.Message == "" || strings.TrimSpace(cmd.Text) == "" {
			return out, ErrInvalid
		}
		out.ID = uuid.NewString()
		return out, tx.Put(bucket(c.ID, "reports"), out.ID, map[string]any{"reporter": p.Key(), "message": cmd.Message, "reason": cmd.Text, "state": "open"})
	case "analyze":
		if s.AI == nil {
			return out, ErrUnavailable
		}
		var r map[string]any
		if e := tx.Get(bucket(c.ID, "reports"), cmd.Target, &r); e != nil {
			return out, e
		}
		messageID, _ := r["message"].(string)
		var message Message
		if e := tx.Get(bucket(c.ID, "messages"), messageID, &message); e != nil {
			return out, e
		}
		if !s.now().Before(message.Expires) || c.RetentionSeconds == 0 {
			return out, ErrNotFound
		}
		if e := s.auth(ctx, p, "groups.messages.read", c); e != nil {
			return out, e
		}
		b, marshalErr := json.Marshal(struct {
			Report  map[string]any
			Message Message
		}{r, message})
		if marshalErr != nil {
			return out, marshalErr
		}
		text, actions, e := s.AI.Analyze(ctx, string(b))
		if e != nil {
			return out, ErrUnavailable
		}
		r["analysis"] = text
		r["suggestions"] = actions
		r["state"] = "suggested"
		out.Detail = text
		return out, tx.Put(bucket(c.ID, "reports"), cmd.Target, r)
	case "feedback", "watch", "ai_execute":
		return s.manageRecord(ctx, tx, p, c, cmd)
	case "preview", "rule":
		return s.rule(ctx, tx, p, c, cmd, cmd.Kind == "preview")
	case "card":
		return s.card(ctx, tx, p, c, cmd)
	case "subscribe":
		rows, err := tx.List(bucket(c.ID, "subscriptions"), "", 100)
		if err != nil {
			return out, err
		}
		var exists map[string]any
		if len(rows) >= 100 && errors.Is(tx.Get(bucket(c.ID, "subscriptions"), cmd.Receiver, &exists), ErrNotFound) {
			return out, ErrInvalid
		}
		var receiver GroupConfig
		if e := tx.Get("config", cmd.Receiver, &receiver); e != nil {
			return out, e
		}
		if e := s.auth(ctx, p, "groups.messages.send", receiver); e != nil {
			return out, e
		}
		out.ID = cmd.Receiver
		return out, tx.Put(bucket(c.ID, "subscriptions"), cmd.Receiver, map[string]any{"receiver": cmd.Receiver, "enabled": cmd.Enabled, "actor": Actor(p)})
	case "warn", "mute", "unmute", "kick", "send", "forward", "recall":
		if cmd.Kind == "forward" {
			if e := s.auth(ctx, p, "groups.messages.read", c); e != nil {
				return out, e
			}
		}
		if cmd.Kind == "forward" || cmd.Kind == "recall" {
			var message Message
			if e := tx.Get(bucket(c.ID, "messages"), cmd.Message, &message); e != nil {
				return out, e
			}
			if c.RetentionSeconds == 0 || !s.now().Before(message.Expires) {
				return out, ErrNotFound
			}
		}

		if cmd.Kind == "mute" && (cmd.Seconds == 0 || cmd.Seconds > 2592000) {
			return out, ErrInvalid
		}
		if slices.Contains([]string{"warn", "mute", "unmute", "kick"}, cmd.Kind) && !valid(cmd.Target, 2048) {
			return out, ErrInvalid
		}
		if (cmd.Kind == "send" || cmd.Kind == "warn") && strings.TrimSpace(cmd.Text) == "" {
			return out, ErrInvalid
		}
		if (cmd.Kind == "forward" || cmd.Kind == "recall") && !valid(cmd.Message, 128) {
			return out, ErrInvalid
		}
		return s.queue(tx, p, c, cmd, cap)
	}
	return out, ErrInvalid
}

type Blacklist struct {
	ID        string    `json:"id"`
	Reason    string    `json:"reason"`
	Until     time.Time `json:"until"`
	Permanent bool      `json:"permanent"`
}

func (s *Service) eligible(ctx context.Context, tx Tx, c GroupConfig, m Member) (bool, error) {
	result, e := s.assessEligibility(ctx, tx, c, m, nil)
	return e == nil && result.state == "eligible", e
}
func (s *Service) admission(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, owner string, apply bool) (Result, error) {
	out := Result{}
	if !valid(owner, 2048) {
		return out, ErrInvalid
	}
	var m Member
	e := tx.Get(bucket(c.ID, "members"), owner, &m)
	fresh := errors.Is(e, ErrNotFound)
	if e != nil && !fresh {
		return out, e
	}
	if m.State == "removed" {
		if !apply {
			return Result{State: "removed"}, nil
		}
		fresh = true
	}
	if fresh {
		if !apply {
			return out, ErrNotFound
		}
		m = Member{Owner: owner, State: "requested"}
	}
	previous := m.State
	ok, e := s.eligible(ctx, tx, c, m)
	if e != nil {
		return out, e
	}
	switch {
	case m.Exempt:
		m.State = "exempt"
	case ok:
		m.State = "restored"
		m.Deadline = time.Time{}
		m.Extended = false
	case fresh:
		m.State = "grace"
		m.Deadline = s.now().Add(time.Duration(c.GraceSeconds) * time.Second)
	case m.Deadline.IsZero():
		m.State = "grace"
		m.Deadline = s.now().Add(time.Duration(c.GraceSeconds) * time.Second)
	}
	if c.Mode == "before" && !ok && (fresh || m.State == "awaiting_verification") {
		m.State = "awaiting_verification"
	}
	if !ok && !m.Exempt && m.State != "awaiting_verification" && m.State != "removed" && m.State != "removal_pending" && !s.now().Before(m.Deadline) {
		r, e := s.queue(tx, p, c, Command{Kind: "kick", Target: owner, Text: "eligibility_expired"}, "admission")
		if e != nil {
			return out, e
		}
		m.State = "removal_pending"
		out = r
	}
	if !ok && !m.Exempt && (m.Reminder.IsZero() || (c.ReminderSeconds > 0 && !s.now().Before(m.Reminder.Add(time.Duration(c.ReminderSeconds)*time.Second)))) {
		if _, e := s.queue(tx, p, c, Command{Kind: "remind", Target: owner, Text: "verification_required"}, "admission"); e != nil {
			return out, e
		}
		m.Reminder = s.now()
	}
	if fresh && c.Mode == "muted" && !ok {
		if _, e := s.queue(tx, p, c, Command{Kind: "mute", Target: owner, Seconds: c.GraceSeconds, Text: "admission"}, "admission"); e != nil {
			return out, e
		}
	}
	if ok && (fresh || previous == "awaiting_verification") && c.Mode == "before" {
		if _, e := s.queue(tx, p, c, Command{Kind: "approve", Target: owner}, "admission"); e != nil {
			return out, e
		}
	}
	if ok && previous != "restored" && c.Mode == "muted" {
		if _, e := s.queue(tx, p, c, Command{Kind: "unmute", Target: owner, Text: "admission_restored"}, "admission"); e != nil {
			return out, e
		}
	}
	if ok && fresh && c.Welcome != "" {
		if _, e := s.queue(tx, p, c, Command{Kind: "welcome", Target: owner, Text: c.Welcome + "\n" + c.RulesTemplate}, "admission"); e != nil {
			return out, e
		}
	}
	out.State = m.State
	return out, tx.Put(bucket(c.ID, "members"), owner, m)
}
func (s *Service) random(n int64) (int64, error) {
	if s.Random != nil {
		return s.Random(n)
	}
	v, e := rand.Int(rand.Reader, big.NewInt(n))
	if e != nil {
		return 0, e
	}
	return v.Int64(), nil
}
func (s *Service) play(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command) (Result, error) {
	out := Result{State: "completed", Action: cmd.Kind}
	if !c.BotEnabled {
		return out, NewErrorDetail(ErrDisabled, "bot_disabled", "bot_enabled", 0)
	}
	var m Member
	if e := tx.Get(bucket(c.ID, "members"), p.Key(), &m); e != nil {
		return out, e
	}
	ok, e := s.eligible(ctx, tx, c, m)
	if e != nil {
		return out, e
	}
	if !m.Joined || !ok || m.State == "removed" {
		return out, ErrForbidden
	}
	if cmd.Kind == "dice" {
		if c.DiceSides < 2 {
			return out, NewErrorDetail(ErrDisabled, "dice_disabled", "dice_sides", 0)
		}
		n, e := s.random(c.DiceSides)
		sides := c.DiceSides
		out.Value = n + 1
		out.Detail = "dice_sides"
		out.MaxValue = &sides
		if e != nil {
			return out, e
		}
		return out, s.audit(tx, p, c.ID, "dice", p.Key(), "completed")
	}
	if c.MuteProbability == 0 {
		return out, NewErrorDetail(ErrDisabled, "self_mute_disabled", "mute_probability", 0)
	}
	now := s.now()
	until := m.LastPlay.Add(time.Duration(c.CooldownSeconds) * time.Second)
	if m.MutedUntil.After(until) {
		until = m.MutedUntil
	}
	if now.Before(until) {
		retryAfter := int64((until.Sub(now) + time.Second - 1) / time.Second)
		if retryAfter < 1 {
			retryAfter = 1
		}
		return out, NewErrorDetail(ErrCooldown, "member_cooldown", "", retryAfter)
	}
	n, e := s.random(100)
	if e != nil {
		return out, e
	}
	m.LastPlay = now
	m.Failures++
	if n < c.MuteProbability || (c.Pity > 0 && m.Failures >= c.Pity) {
		m.Failures = 0
		out, e = s.queue(tx, p, c, Command{Kind: "mute", Target: p.Key(), Seconds: c.MuteSeconds, Text: "entertainment"}, "entertainment")
		if e != nil {
			return out, e
		}
		secs := c.MuteSeconds
		out.Seconds = &secs
		out.Detail = "self_mute_triggered"
		if c.Pity > 0 {
			zero := int64(0)
			out.PityRemaining = &zero
		}
		m.MutedUntil = now.Add(time.Duration(c.MuteSeconds) * time.Second)
	} else {
		zero := int64(0)
		out.Seconds = &zero
		out.Detail = "self_mute_missed"
		if c.Pity > 0 {
			rem := c.Pity - m.Failures
			if rem < 0 {
				rem = 0
			}
			out.PityRemaining = &rem
		}
	}
	return out, tx.Put(bucket(c.ID, "members"), p.Key(), m)
}
