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
func (s *Service) auth(ctx context.Context, p account.Principal, c string, g Config) error {
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
		return false, ErrConflict
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
func validateConfig(c Config) error {
	if c.ID == "*" || c.School == "*" || !valid(c.ID, 128) || !valid(c.School, 128) || !slices.Contains([]string{"before", "muted", "password"}, c.Mode) {
		return ErrInvalid
	}
	for _, n := range []int64{c.GraceSeconds, c.ReminderSeconds, c.RetentionSeconds, c.CooldownSeconds} {
		if n < 0 || n > 31536000 {
			return ErrInvalid
		}
	}
	if c.DiceSides < 0 || c.DiceSides == 1 || c.DiceSides > 1000000 || c.MuteProbability < 0 || c.MuteProbability > 100 || c.MuteSeconds < 0 || c.MuteSeconds > 2592000 || c.Pity < 0 || c.Pity > 10000 || c.RepeatThreshold < 0 || c.RepeatThreshold > 1000 || len(c.Keywords) > 100 || len(c.CardTemplate) > 1024 {
		return ErrInvalid
	}
	if len(c.Welcome) > 2048 || len(c.RulesTemplate) > 8192 || c.FrequencySeconds < 0 || c.FrequencySeconds > 86400 || c.FrequencyLimit < 0 || c.FrequencyLimit > 10000 || c.WarningLimit < 0 || c.WarningLimit > 10000 {
		return ErrInvalid
	}
	if c.GraceSeconds == 0 {
		return ErrInvalid
	}
	if c.MuteProbability > 0 && (c.MuteSeconds == 0 || c.CooldownSeconds == 0) {
		return ErrInvalid
	}
	if c.Mode == "password" {
		b, e := hex.DecodeString(c.PasswordHash)
		if e != nil || len(b) != 32 {
			return ErrInvalid
		}
	}
	for _, k := range c.Keywords {
		if strings.TrimSpace(k) == "" || len(k) > 256 {
			return ErrInvalid
		}
	}
	if slices.Contains(c.AutoActions, "mute") && c.MuteSeconds == 0 {
		return ErrInvalid
	}
	for _, a := range c.AutoActions {
		if !slices.Contains([]string{"warn", "recall", "mute", "kick", "blacklist"}, a) {
			return ErrInvalid
		}
	}
	return nil
}
func (s *Service) Configure(ctx context.Context, p account.Principal, key string, c Config) (Config, error) {
	if c.Keywords == nil {
		c.Keywords = []string{}
	}
	if c.AutoActions == nil {
		c.AutoActions = []string{}
	}
	if e := validateConfig(c); e != nil {
		return Config{}, e
	}
	if !valid(key, 128) {
		return Config{}, ErrInvalid
	}
	var out Config
	e := s.run(ctx, func(tx Tx) error {
		if e := s.auth(ctx, p, "groups.configure", c); e != nil {
			return e
		}
		var old Config
		e := tx.Get("config", c.ID, &old)
		if e == nil && old.School != c.School {
			return ErrConflict
		}
		if e != nil && !errors.Is(e, ErrNotFound) {
			return e
		}
		ok, e := replay(tx, p.Key(), key, c, &out)
		if e != nil || ok {
			return e
		}
		out = c
		if e = tx.Put("config", c.ID, c); e != nil {
			return e
		}
		if e = s.audit(tx, p, c.ID, "configure", c.ID, "saved"); e != nil {
			return e
		}
		if e = s.event(tx, c.ID, "configured", map[string]string{"group": c.ID}); e != nil {
			return e
		}
		return save(tx, p.Key(), key, c, out)
	})
	return out, e
}
func (s *Service) Configuration(ctx context.Context, p account.Principal, g string) (Config, error) {
	var c Config
	e := s.run(ctx, func(tx Tx) error {
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		return s.auth(ctx, p, "groups.read", c)
	})
	c.PasswordHash = ""
	return c, e
}
func (s *Service) List(ctx context.Context, p account.Principal, g, collection, cursor string, limit int) (Page, error) {
	out := Page{Items: []json.RawMessage{}}
	if limit < 1 || limit > 100 || !slices.Contains([]string{"members", "actions", "reports", "hits", "messages", "subscriptions", "blacklist", "warnings", "watch", "deliveries"}, collection) {
		return out, ErrInvalid
	}
	e := s.run(ctx, func(tx Tx) error {
		var c Config
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		cap := "groups.read"
		if collection == "messages" {
			cap = "groups.messages.read"
		}
		if e := s.auth(ctx, p, cap, c); e != nil {
			return e
		}
		rows, e := tx.List(bucket(g, collection), cursor, limit)
		if e != nil {
			return e
		}
		for _, r := range rows {
			out.NextCursor = r.ID
			if collection == "messages" {
				var m Message
				if e = json.Unmarshal(r.Data, &m); e != nil {
					return e
				}
				if c.RetentionSeconds == 0 || !s.now().Before(m.Expires) {
					continue
				}
			}
			var record map[string]any
			if e = json.Unmarshal(r.Data, &record); e != nil {
				return e
			}
			record["id"] = r.ID
			if collection == "deliveries" {
				receiver, _ := record["receiver"].(string)
				id, _ := record["action"].(string)
				var action Action
				if e = tx.Get(bucket(receiver, "actions"), id, &action); e != nil {
					return e
				}
				record["state"] = action.State
			}
			encoded, e := json.Marshal(record)
			if e != nil {
				return e
			}
			out.Items = append(out.Items, encoded)
		}
		if len(rows) < limit {
			out.NextCursor = ""
		}
		return nil
	})
	return out, e
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
func (s *Service) queue(tx Tx, p account.Principal, c Config, cmd Command, cap string) (Result, error) {
	pending, e := tx.PendingActions(c.ID)
	if e != nil {
		return Result{}, e
	}
	if pending >= 1000 {
		return Result{}, ErrCooldown
	}
	a := Action{ID: uuid.NewString(), Group: c.ID, Actor: p, Capability: cap, Command: cmd, State: "pending", Created: s.now()}
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
		var c Config
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
func (s *Service) command(ctx context.Context, tx Tx, p account.Principal, c Config, cmd Command, cap string) (Result, error) {
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
			return out, ErrDisabled
		}
		h := sha256.Sum256([]byte(cmd.Text))
		if hex.EncodeToString(h[:]) != c.PasswordHash {
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
		if e := tx.Put(bucket(c.ID, "blacklist"), cmd.Target, Blacklist{cmd.Text, s.now().Add(time.Duration(cmd.Seconds) * time.Second), cmd.Seconds == 0}); e != nil {
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
		var receiver Config
		if e := tx.Get("config", cmd.Receiver, &receiver); e != nil {
			return out, e
		}
		if e := s.auth(ctx, p, "groups.messages.send", receiver); e != nil {
			return out, e
		}
		out.ID = cmd.Receiver
		return out, tx.Put(bucket(c.ID, "subscriptions"), cmd.Receiver, map[string]any{"receiver": cmd.Receiver, "enabled": cmd.Enabled, "actor": p})
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
	Reason    string
	Until     time.Time
	Permanent bool
}

func (s *Service) eligible(ctx context.Context, tx Tx, c Config, m Member) (bool, error) {
	for _, g := range []string{c.ID, "*"} {
		var b Blacklist
		e := tx.Get(bucket(g, "blacklist"), m.Owner, &b)
		if e == nil && (b.Permanent || s.now().Before(b.Until)) {
			return false, nil
		}
		if e != nil && !errors.Is(e, ErrNotFound) {
			return false, e
		}
	}
	if s.Facts == nil {
		return false, ErrUnavailable
	}
	f, e := s.currentFacts(ctx, m.Owner, c.School)
	if e != nil {
		return false, ErrUnavailable
	}
	return f.Bound && (!c.RequireStudent || (f.StudentKnown && f.StageKnown && f.Student && (!f.Alumni || c.AcceptAlumni) && (!f.Temporary || c.AcceptTemporary))) && (c.Mode != "password" || m.Password), nil
}
func (s *Service) admission(ctx context.Context, tx Tx, p account.Principal, c Config, owner string, apply bool) (Result, error) {
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
func (s *Service) play(ctx context.Context, tx Tx, p account.Principal, c Config, cmd Command) (Result, error) {
	out := Result{State: "completed", Action: cmd.Kind}
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
			return out, ErrDisabled
		}
		n, e := s.random(c.DiceSides)
		out.Value = n + 1
		out.Detail = "dice_sides"
		if e != nil {
			return out, e
		}
		return out, s.audit(tx, p, c.ID, "dice", p.Key(), "completed")
	}
	if c.MuteProbability == 0 {
		return out, ErrDisabled
	}
	if s.now().Before(m.LastPlay.Add(time.Duration(c.CooldownSeconds)*time.Second)) || s.now().Before(m.MutedUntil) {
		return out, ErrCooldown
	}
	n, e := s.random(100)
	if e != nil {
		return out, e
	}
	m.LastPlay = s.now()
	m.Failures++
	if n < c.MuteProbability || (c.Pity > 0 && m.Failures >= c.Pity) {
		m.Failures = 0
		out, e = s.queue(tx, p, c, Command{Kind: "mute", Target: p.Key(), Seconds: c.MuteSeconds, Text: "entertainment"}, "entertainment")
		if e != nil {
			return out, e
		}
		m.MutedUntil = s.now().Add(time.Duration(c.MuteSeconds) * time.Second)
	}
	return out, tx.Put(bucket(c.ID, "members"), p.Key(), m)
}
