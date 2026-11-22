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
func validText(v string, n int) bool {
	return utf8.ValidString(v) && !strings.ContainsRune(v, 0) && utf8.RuneCountInString(v) <= n
}
func validCursor(v string, n int) bool {
	return validText(v, n) && !strings.ContainsFunc(v, unicode.IsControl)
}
func valid(v string, n int) bool {
	return strings.TrimSpace(v) != "" && validCursor(v, n) && !strings.Contains(v, "/")
}
func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func bucket(g, k string) string { return g + "/" + k }
func (s *Service) run(ctx context.Context, fn func(Tx) error) error {
	return s.runScoped(ctx, nil, fn)
}
func (s *Service) runScoped(ctx context.Context, scopes []string, fn func(Tx) error) error {
	if s.Repo == nil {
		return ErrUnavailable
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.Repo.ScopedTransaction(c, scopes, fn)
}
func (s *Service) runRead(ctx context.Context, fn func(ReadTx) error) error {
	if s.Repo == nil {
		return ErrUnavailable
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.Repo.ReadOnly(c, fn)
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
func (s *Service) event(ctx context.Context, tx Tx, g, kind string, data any) error {
	b, e := json.Marshal(data)
	if e != nil {
		return e
	}
	return tx.Emit(Event{SpecVersion: "1.0", ID: uuid.NewString(), Source: "groups", Type: "groups." + kind, Subject: g, TraceParent: TraceParentFromContext(ctx), Time: s.now(), Data: b})
}
func (s *Service) audit(tx Tx, p account.Principal, g, kind, target, result string) error {
	actor := p.Key()
	if actor == "" {
		actor = "system:groups"
	}
	return tx.Put(bucket(g, "audit"), uuid.NewString(), map[string]any{"actor": actor, "action": kind, "target": target, "result": result, "time": s.now()})
}

type receipt struct {
	Digest    string
	Result    json.RawMessage
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func validKey(v string) bool {
	if len(v) < 1 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < ' ' || v[i] > '~' {
			return false
		}
	}
	return true
}

func replay(tx ReadTx, owner, operation, object, key string, input, out any) (bool, error) {
	hit, err := replayReceipt(tx, hash([]string{owner, operation, object, key}), input, out)
	if hit || err != nil {
		return hit, err
	}
	// Legacy receipts have only the digest to identify the original operation.
	return replayReceipt(tx, hash([]string{owner, key}), input, out)
}

func replayReceipt(tx ReadTx, id string, input, out any) (bool, error) {
	var r receipt
	e := tx.Get("receipts", id, &r)
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
func (s *Service) saveReceipt(tx Tx, owner, operation, object, key string, input, out any) error {
	b, e := json.Marshal(out)
	if e != nil {
		return e
	}
	return tx.Put("receipts", hash([]string{owner, operation, object, key}), receipt{Digest: hash(input), Result: b, CreatedAt: s.now()})
}
func validateConfig(c GroupConfig) error {
	if c.ID == "directory" || c.ID == "me" || c.ID == "*" || !valid(c.ID, 128) {
		return NewErrorDetail(ErrInvalid, "invalid_field", "id", 0)
	}
	if !slices.Contains([]string{"", "school", "department", "grade", "course", "club"}, c.Category) || !utf8.ValidString(c.Description) || utf8.RuneCountInString(c.Description) > 512 || strings.ContainsFunc(c.Description, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) || !utf8.ValidString(c.PasswordHint) || utf8.RuneCountInString(c.PasswordHint) > 256 || strings.ContainsFunc(c.PasswordHint, unicode.IsControl) {
		return ErrInvalid
	}
	if c.School == "*" || !valid(c.School, 128) {
		return NewErrorDetail(ErrInvalid, "invalid_field", "school", 0)
	}
	if strings.TrimSpace(c.Name) == "" || !validText(c.Name, 128) || strings.ContainsFunc(c.Name, unicode.IsControl) {
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
	if !validText(c.CardTemplate, 1024) {
		return NewErrorDetail(ErrInvalid, "invalid_field", "card_template", 0)
	}
	if _, _, err := renderCardTemplate(c.CardTemplate, nil); err != nil {
		return NewErrorDetail(ErrInvalid, "invalid_field", "card_template", 0)
	}
	if !validText(c.Welcome, 2048) {
		return NewErrorDetail(ErrInvalid, "invalid_field", "welcome", 0)
	}
	if !validText(c.RulesTemplate, 8192) {
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
		if strings.TrimSpace(k) == "" || !validText(k, 256) {
			return NewErrorDetail(ErrInvalid, "invalid_field", "keywords", 0)
		}
	}
	if slices.Contains(c.AutoActions, "mute") && c.MuteSeconds == 0 {
		return NewErrorDetail(ErrInvalid, "missing_mute_seconds", "mute_seconds", 0)
	}
	for i, a := range c.AutoActions {
		if slices.Contains(c.AutoActions[:i], a) {
			return ErrInvalid
		}
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
	case "extend", "exempt", "unexempt", "warn", "mute", "unmute", "kick", "blacklist", "unblacklist", "cancel_action", "retry_action":
		return "groups.manage"
	case "subscribe", "rule", "ai_execute":
		return "groups.configure"
	case "feedback", "watch":
		return "groups.manage"
	case "analyze", "preview", "card", "card_preview", "recheck":
		return "groups.read"
	}
	return ""
}
func (s *Service) queue(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command, cap string) (Result, error) {
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
	if tp := TraceParentFromContext(ctx); tp != "" {
		if e := tx.Put(bucket(c.ID, "action_traces"), a.ID, tp); e != nil {
			return Result{}, e
		}
	}
	if e := s.audit(tx, p, c.ID, cmd.Kind, cmd.Target, "pending"); e != nil {
		return Result{}, e
	}
	if e := s.event(ctx, tx, c.ID, "action.requested", map[string]string{"id": a.ID}); e != nil {
		return Result{}, e
	}
	return Result{ID: a.ID, State: "pending", Action: cmd.Kind}, nil
}
func (s *Service) Command(ctx context.Context, p account.Principal, g, key string, cmd Command) (Result, error) {
	var out Result
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validKey(key) || !valid(g, 128) || !validText(cmd.Text, 8192) || !validText(cmd.Message, 128) || !validText(cmd.Receiver, 128) || !validText(cmd.Target, 2048) || cmd.Seconds < 0 || cmd.Seconds > 31536000 {
		return out, ErrInvalid
	}
	input := struct {
		Group   string
		Command Command
	}{g, cmd}
	if cmd.Kind == "analyze" {
		return s.executeAnalyze(ctx, p, g, key, cmd, input)
	}
	scopes := []string{"group:" + g, "receipt:" + p.Key() + ":" + key}
	if cmd.Kind == "apply" {
		scopes = append(scopes, "owner:"+p.Key())
	}
	if cmd.Kind == "subscribe" && cmd.Receiver != "" {
		scopes = append(scopes, "group:"+cmd.Receiver)
	}
	e := s.runPrepared(ctx, scopes, func(tx *preparedTx) error {
		if cmd.Kind == "report" {
			if !valid(cmd.Message, 128) {
				return ErrInvalid
			}
			var m Member
			err := tx.Get(bucket(g, "members"), p.Key(), &m)
			if errors.Is(err, ErrNotFound) {
				return ErrForbidden
			}
			if err != nil {
				return err
			}
			if !m.Joined || m.State == "removed" {
				return ErrForbidden
			}
		}
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
		ok, e := replay(tx, p.Key(), "command", g, key, input, &out)
		if e != nil || ok {
			return e
		}
		out, e = s.command(ctx, tx, p, c, cmd, cap)
		if e != nil {
			return e
		}
		return s.saveReceipt(tx, p.Key(), "command", g, key, input, out)
	})
	return out, e
}
func (s *Service) executeAnalyze(ctx context.Context, p account.Principal, g, key string, cmd Command, input any) (Result, error) {
	out := Result{State: "completed"}
	var replayed bool
	var payload []byte
	preErr := s.runRead(ctx, func(tx ReadTx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		if e := s.auth(ctx, p, "groups.read", c); e != nil {
			return e
		}
		ok, e := replay(tx, p.Key(), "command", g, key, input, &out)
		if e != nil || ok {
			replayed = ok
			return e
		}
		if s.AI == nil {
			return ErrUnavailable
		}
		var r map[string]any
		if e := tx.Get(bucket(c.ID, "reports"), cmd.Target, &r); e != nil {
			return e
		}
		messageID, _ := r["message"].(string)
		var message Message
		if e := tx.Get(bucket(c.ID, "messages"), messageID, &message); e != nil {
			return e
		}
		if !s.now().Before(message.Expires) || c.RetentionSeconds == 0 {
			return ErrNotFound
		}
		if e := s.auth(ctx, p, "groups.messages.read", c); e != nil {
			return e
		}
		if r["state"] == "pending_execution" || r["state"] == "resolved" {
			return ErrConflict
		}
		reason, _ := r["reason"].(string)
		var marshalErr error
		payload, marshalErr = json.Marshal(struct {
			Reason string `json:"reason"`
			Body   string `json:"body"`
		}{reason, message.Body})
		return marshalErr
	})
	if preErr != nil || replayed {
		return out, preErr
	}

	// Invoke external AI outside any database transaction or advisory lock with a bounded 5s timeout.
	aiCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	text, actions, err := s.AI.Analyze(aiCtx, string(payload))
	if err != nil {
		return out, ErrUnavailable
	}

	e := s.runScoped(ctx, []string{"group:" + g, "receipt:" + p.Key() + ":" + key}, func(tx Tx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		if e := s.auth(ctx, p, "groups.read", c); e != nil {
			return e
		}
		ok, e := replay(tx, p.Key(), "command", g, key, input, &out)
		if e != nil || ok {
			return e
		}
		var r map[string]any
		if e := tx.Get(bucket(c.ID, "reports"), cmd.Target, &r); e != nil {
			return e
		}
		if state, _ := r["state"].(string); state == "resolved" || state == "pending_execution" {
			return ErrConflict
		}
		messageID, _ := r["message"].(string)
		var message Message
		if e := tx.Get(bucket(c.ID, "messages"), messageID, &message); e != nil {
			return e
		}
		if !s.now().Before(message.Expires) || c.RetentionSeconds == 0 {
			return ErrNotFound
		}
		if e := s.auth(ctx, p, "groups.messages.read", c); e != nil {
			return e
		}
		r["updated"] = s.now()
		r["analysis"] = text
		r["suggestions"] = actions
		r["state"] = "suggested"
		out.Detail = text
		if e := tx.Put(bucket(c.ID, "reports"), cmd.Target, r); e != nil {
			return e
		}
		return s.saveReceipt(tx, p.Key(), "command", g, key, input, out)
	})
	return out, e
}
func (s *Service) command(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command, cap string) (Result, error) {
	out := Result{State: "completed"}
	switch cmd.Kind {
	case "apply", "recheck":
		trigger := checkAdmission
		if cmd.Kind == "apply" {
			trigger = applyAdmission
		}
		return s.admission(ctx, tx, p, c, cmd.Target, trigger)
	case "extend", "exempt", "unexempt":
		var m Member
		if e := tx.Get(bucket(c.ID, "members"), cmd.Target, &m); e != nil {
			return out, e
		}
		if strings.TrimSpace(cmd.Text) == "" {
			return out, ErrInvalid
		}
		if m.State == "removed" {
			return out, ErrConflict
		}
		switch cmd.Kind {
		case "extend":
			if cmd.Seconds == 0 {
				return out, ErrInvalid
			}
			base := s.now()
			if m.Deadline.After(base) {
				base = m.Deadline
			}
			m.Deadline = base.Add(time.Duration(cmd.Seconds) * time.Second)
			m.Extended = true
			m.State = "extended"
		case "exempt":
			m.Exempt = true
			m.Reminder = time.Time{}
			m.Extended = false
			m.Deadline = time.Time{}
			m.State = "exempt"
		case "unexempt":
			if !m.Exempt {
				return Result{State: "noop", Action: cmd.Kind, Detail: m.State}, nil
			}
			m.Exempt = false
			m.Extended = false
			m.Deadline = time.Time{}
			if m.State == "exempt" {
				if !m.Joined && c.Mode == "before" {
					m.State = "awaiting_verification"
				} else {
					m.State = "grace"
				}
			}
			if e := tx.Put(bucket(c.ID, "members"), cmd.Target, m); e != nil {
				return out, e
			}
			var adm Result
			if c.BotEnabled {
				var e error
				adm, e = s.admission(ctx, tx, p, c, cmd.Target, checkAdmission)
				if e != nil {
					return out, e
				}
			} else {
				ok, e := s.eligible(ctx, tx, c, m)
				if e != nil {
					return out, e
				}
				switch {
				case ok:
					m.State = "restored"
				case !m.Joined && c.Mode == "before":
					m.State = "awaiting_verification"
				default:
					m.State = "grace"
					m.Deadline = s.now().Add(time.Duration(c.GraceSeconds) * time.Second)
				}
				if e := tx.Put(bucket(c.ID, "members"), cmd.Target, m); e != nil {
					return out, e
				}
				adm = Result{State: m.State}
			}
			adm.Action = "unexempt"
			return adm, s.audit(tx, p, c.ID, "unexempt", cmd.Target, adm.State)
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
		m.PasswordEpoch = stored.PasswordEpoch
		if e := tx.Put(bucket(c.ID, "members"), cmd.Target, m); e != nil {
			return out, e
		}
		return s.admission(ctx, tx, p, c, cmd.Target, checkAdmission)
	case "dice", "self_mute":
		return s.play(ctx, tx, p, c, cmd)
	case "blacklist":
		if strings.TrimSpace(cmd.Text) == "" {
			return out, ErrInvalid
		}
		if !valid(cmd.Target, 2048) {
			return out, ErrInvalid
		}
		now := s.now()
		created := now
		var prior Blacklist
		if e := tx.Get(bucket(c.ID, "blacklist"), cmd.Target, &prior); e == nil && prior.Created != nil && !prior.Created.IsZero() {
			created = *prior.Created
		}
		if e := tx.Put(bucket(c.ID, "blacklist"), cmd.Target, Blacklist{ID: cmd.Target, Reason: cmd.Text, Until: now.Add(time.Duration(cmd.Seconds) * time.Second), Permanent: cmd.Seconds == 0, Created: &created, Updated: &now}); e != nil {
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
		now := s.now()
		out.ID = uuid.NewString()
		record := map[string]any{"reporter": p.Key(), "message": cmd.Message, "reason": cmd.Text, "state": "open", "created": now, "updated": now}
		var msg Message
		if e := tx.Get(bucket(c.ID, "messages"), cmd.Message, &msg); e == nil {
			if c.RetentionSeconds > 0 && now.Before(msg.Expires) {
				record["target"] = msg.Owner
			}
		} else if !errors.Is(e, ErrNotFound) {
			return out, e
		}
		return out, tx.Put(bucket(c.ID, "reports"), out.ID, record)
	case "feedback", "watch", "ai_execute":
		return s.manageRecord(ctx, tx, p, c, cmd)
	case "preview", "rule":
		return s.rule(ctx, tx, p, c, cmd, cmd.Kind == "preview")
	case "card":
		return s.card(ctx, tx, p, c, cmd)
	case "cancel_action", "retry_action":
		return s.manageAction(ctx, tx, p, c, cmd)
	case "card_preview":
		return s.cardPreview(ctx, tx, c, cmd)
	case "subscribe":
		if !valid(cmd.Receiver, 128) {
			return out, ErrInvalid
		}
		var prior Subscription
		err := tx.Get(bucket(c.ID, "subscriptions"), cmd.Receiver, &prior)
		exists := err == nil
		if err != nil && !errors.Is(err, ErrNotFound) {
			return out, err
		}
		state := "disabled"
		if cmd.Enabled {
			var receiver GroupConfig
			if err := tx.Get("config", cmd.Receiver, &receiver); err != nil {
				return out, err
			}
			if err := s.auth(ctx, p, "groups.messages.send", receiver); err != nil {
				return out, err
			}
			if !exists {
				rows, err := tx.List(bucket(c.ID, "subscriptions"), "", 100)
				if err != nil {
					return out, err
				}
				if len(rows) >= 100 {
					return out, ErrInvalid
				}
			}
			state = "enabled"
		} else if !exists {
			return out, ErrNotFound
		}
		out.ID = cmd.Receiver
		if err := tx.Put(bucket(c.ID, "subscriptions"), cmd.Receiver, map[string]any{"receiver": cmd.Receiver, "enabled": cmd.Enabled, "actor": Actor(p)}); err != nil {
			return out, err
		}
		return out, s.audit(tx, p, c.ID, "subscribe", cmd.Receiver, state)
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
		return s.queue(ctx, tx, p, c, cmd, cap)
	}
	return out, ErrInvalid
}

type Blacklist struct {
	ID        string     `json:"id"`
	Reason    string     `json:"reason"`
	Until     time.Time  `json:"until"`
	Permanent bool       `json:"permanent"`
	Created   *time.Time `json:"created,omitempty"`
	Updated   *time.Time `json:"updated,omitempty"`
}

func (s *Service) eligible(ctx context.Context, tx Tx, c GroupConfig, m Member) (bool, error) {
	result, e := s.assessEligibility(ctx, tx, c, m, nil)
	return e == nil && result.state == "eligible", e
}

type admissionTrigger int

const (
	checkAdmission admissionTrigger = iota
	applyAdmission
	joinAdmission
)

// Stored separately from the public Member response, for this membership only.
type admissionHistory struct{ Admitted, Muted bool }

func (s *Service) admission(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, owner string, trigger admissionTrigger) (Result, error) {
	return s.admissionWithFacts(ctx, tx, p, c, owner, trigger, nil)
}
func (s *Service) admissionWithFacts(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, owner string, trigger admissionTrigger, cache map[factKey]Facts) (Result, error) {
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
		if trigger == checkAdmission {
			return Result{State: "removed"}, nil
		}
		fresh = true
	}
	if fresh {
		if trigger == checkAdmission {
			return out, ErrNotFound
		}
		m = Member{Owner: owner, State: "requested"}
	}
	joinedNow := trigger == joinAdmission && !m.Joined
	var history admissionHistory
	if m.Joined {
		err := tx.Get(bucket(c.ID, "admissions"), owner, &history)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return out, err
		}
		// Existing restored members were already admitted before this marker existed.
		if errors.Is(err, ErrNotFound) && m.State == "restored" {
			history.Admitted = true
		}
	}
	if joinedNow {
		m.Joined = true
		m.State = "requested"
		m.Deadline = time.Time{}
		m.Reminder = time.Time{}
		if err := tx.Delete(bucket(c.ID, "admissions"), owner); err != nil {
			return out, err
		}
	}
	previous := m.State
	assessment, e := s.assessEligibility(ctx, tx, c, m, cache)
	ok := assessment.state == "eligible"
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
		if !m.Joined && previous != "restored" && c.Mode == "before" {
			if _, e := s.queue(ctx, tx, p, c, Command{Kind: "approve", Target: owner}, "admission"); e != nil {
				return out, e
			}
		}
		if m.Joined && history.Muted {
			if _, e := s.queue(ctx, tx, p, c, Command{Kind: "unmute", Target: owner, Text: "admission_restored"}, "admission"); e != nil {
				return out, e
			}
			history.Muted = false
		}
		if m.Joined && !history.Admitted && c.Welcome != "" {
			if _, e := s.queue(ctx, tx, p, c, Command{Kind: "welcome", Target: owner, Text: c.Welcome + "\n" + c.RulesTemplate}, "admission"); e != nil {
				return out, e
			}
		}
		history.Admitted = m.Joined
	case !m.Joined:
		m.State = "awaiting_verification"
		m.Deadline = time.Time{}
		m.Reminder = time.Time{}
	default:
		if m.Deadline.IsZero() {
			m.State = "grace"
			m.Deadline = s.now().Add(time.Duration(c.GraceSeconds) * time.Second)
		}
		if m.State == "removal_pending" {
			pending, err := tx.PendingAdmissionKick(c.ID, owner)
			if err != nil {
				return out, err
			}
			if !pending {
				m.State = "grace"
			}
		}
		if m.State != "removal_pending" && !s.now().Before(m.Deadline) {
			out, e = s.queue(ctx, tx, p, c, Command{Kind: "kick", Target: owner, Text: "eligibility_expired"}, "admission")
			if e != nil {
				return out, e
			}
			m.State = "removal_pending"
		}
		if m.State != "removal_pending" && (m.Reminder.IsZero() || c.ReminderSeconds > 0 && !s.now().Before(m.Reminder.Add(time.Duration(c.ReminderSeconds)*time.Second))) {
			if _, e := s.queue(ctx, tx, p, c, Command{Kind: "remind", Target: owner, Text: "verification_required"}, "admission"); e != nil {
				return out, e
			}
			m.Reminder = s.now()
		}
		if joinedNow && c.Mode == "muted" {
			if _, e := s.queue(ctx, tx, p, c, Command{Kind: "mute", Target: owner, Seconds: c.GraceSeconds, Text: "admission"}, "admission"); e != nil {
				return out, e
			}
			history.Muted = true
		}
	}
	if m.Joined {
		if err := tx.Put(bucket(c.ID, "admissions"), owner, history); err != nil {
			return out, err
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
	ok, e := s.canPlay(ctx, tx, c, m)
	if e != nil {
		return out, e
	}
	if !ok {
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
	var pending string
	if err := tx.Get(bucket(c.ID, "self_mutes"), m.Owner, &pending); err == nil {
		return out, NewErrorDetail(ErrCooldown, "member_cooldown", "", 0)
	} else if !errors.Is(err, ErrNotFound) {
		return out, err
	}
	n, e := s.random(100)
	if e != nil {
		return out, e
	}
	m.LastPlay = now
	m.Failures++
	if n < c.MuteProbability || (c.Pity > 0 && m.Failures >= c.Pity) {
		m.Failures = 0
		out, e = s.queue(ctx, tx, p, c, Command{Kind: "mute", Target: p.Key(), Seconds: c.MuteSeconds, Text: "entertainment"}, "entertainment")
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
		if err := tx.Put(bucket(c.ID, "self_mutes"), m.Owner, out.ID); err != nil {
			return out, err
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
