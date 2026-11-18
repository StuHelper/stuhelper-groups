package groups

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/StuHelper/stuhelper-platform/account"
	"github.com/google/uuid"
)

type Message struct {
	Attachments []string  `json:"attachments"`
	ID          string    `json:"id"`
	Owner       string    `json:"owner"`
	Body        string    `json:"body"`
	Recalled    bool      `json:"recalled"`
	Expires     time.Time `json:"expires"`
}

// Ingest is a trusted backend boundary, never mounted as a public route.
// Only authenticated bot adapters may provide the actual group and sender.
func (s *Service) Ingest(ctx context.Context, g string, event Event, m Message) error {
	stream := g + "/messages/" + m.ID
	if !valid(g, 128) || event.Source != "qq-bot" || (event.TraceParent != "" && !validTraceParent(event.TraceParent)) || (event.Subject != g && event.Subject != stream) || event.Type != "groups.message" || !valid(m.ID, 128) || !valid(m.Owner, 2048) || !validText(m.Body, 8192) {
		return ErrInvalid
	}
	if event.TraceParent != "" && TraceParentFromContext(ctx) == "" {
		ctx = WithTraceParent(ctx, event.TraceParent)
	}
	if len(m.Attachments) > 20 {
		return ErrInvalid
	}
	for _, ref := range m.Attachments {
		if !valid(ref, 128) {
			return ErrInvalid
		}
	}
	if m.Attachments == nil {
		m.Attachments = []string{}
	}
	return s.runScoped(ctx, []string{"group:" + g}, func(tx Tx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		payload, err := json.Marshal(struct {
			EventData json.RawMessage
			Message   Message
		}{event.Data, m})
		if err != nil {
			return err
		}
		event.revisionSubject = stream
		event.Data = payload
		ok, e := tx.Accept(event)
		if e != nil || !ok {
			return e
		}
		if c.RetentionSeconds == 0 {
			return nil
		}
		m.Expires = s.now().Add(time.Duration(c.RetentionSeconds) * time.Second)
		var previous Message
		if err := tx.Get(bucket(g, "messages"), m.ID, &previous); err == nil {
			if !s.now().Before(previous.Expires) {
				return tx.Delete(bucket(g, "messages"), m.ID)
			}
			m.Expires = previous.Expires
			if m.Recalled {
				m.Owner = previous.Owner
				if m.Body == "" {
					m.Body = previous.Body
				}
				if len(m.Attachments) == 0 && len(previous.Attachments) > 0 {
					m.Attachments = previous.Attachments
				}
			}
			if previous.Recalled {
				m.Recalled = true
			}
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if e = tx.Put(bucket(g, "messages"), m.ID, m); e != nil {
			return e
		}
		return s.event(ctx, tx, g, "message.changed", map[string]string{"id": m.ID})
	})
}

func (s *Service) rule(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command, preview bool) (Result, error) {
	out := Result{State: "clear"}
	hits := []string{}
	text := strings.ToLower(cmd.Text)
	if c.BlockLinks && (strings.Contains(text, "http://") || strings.Contains(text, "https://")) {
		hits = append(hits, "link")
	}
	exceeded, err := s.frequency(tx, c, cmd, preview)
	if err != nil {
		return out, err
	}
	if exceeded {
		hits = append(hits, "frequency")
	}

	for _, word := range c.Keywords {
		if strings.Contains(cmd.Text, word) {
			hits = append(hits, "keyword")
			break
		}
	}
	var previous struct {
		Text  string
		Count int64
	}
	e := tx.Get(bucket(c.ID, "repeat"), cmd.Target, &previous)
	if e != nil && !errors.Is(e, ErrNotFound) {
		return out, e
	}
	if previous.Text == cmd.Text {
		previous.Count++
	} else {
		previous.Text = cmd.Text
		previous.Count = 1
	}
	if c.RepeatThreshold > 0 && previous.Count >= c.RepeatThreshold {
		hits = append(hits, "repeat")
	}
	if !preview {
		if e = tx.Put(bucket(c.ID, "repeat"), cmd.Target, previous); e != nil {
			return out, e
		}
	}
	if len(hits) == 0 {
		return out, nil
	}
	out.State = "suggested"
	out.Detail = strings.Join(hits, ",")
	if preview {
		return out, nil
	}
	out.ID = uuid.NewString()
	if e = tx.Put(bucket(c.ID, "hits"), out.ID, map[string]any{"owner": cmd.Target, "message": cmd.Message, "rules": hits, "state": "matched"}); e != nil {
		return out, e
	}
	for i, a := range c.AutoActions {
		if slices.Contains(c.AutoActions[:i], a) {
			continue
		}
		cap := capability(a)
		if e = s.auth(ctx, p, cap, c); e != nil {
			return out, e
		}
		command := Command{Kind: a, Target: cmd.Target, Message: cmd.Message, Text: "rule:" + out.Detail}
		if a == "mute" {
			command.Seconds = c.MuteSeconds
		}
		if a == "blacklist" {
			if _, e = s.command(ctx, tx, p, c, command, cap); e != nil {
				return out, e
			}
		} else {
			if _, e = s.queue(ctx, tx, p, c, command, cap); e != nil {
				return out, e
			}
		}
	}
	return out, nil
}

var cardTemplateFields = []string{"school", "grade", "department", "name", "student_id", "gender", "nickname"}

func renderCardTemplate(tmpl string, fields map[string]CardField) (string, []string, error) {
	var b strings.Builder
	missing := []string{}
	for i := 0; i < len(tmpl); {
		switch tmpl[i] {
		case '}':
			return "", nil, ErrInvalid
		case '{':
			end := strings.IndexByte(tmpl[i+1:], '}')
			if end < 0 {
				return "", nil, ErrInvalid
			}
			token := tmpl[i+1 : i+1+end]
			i += end + 2
			if token == "separator" {
				sep := "·"
				if fields["gender"].Value == "male" {
					sep = "-"
				} else if fields["gender"].Value == "female" {
					sep = "+"
				}
				b.WriteString(sep)
				continue
			}
			if !slices.Contains(cardTemplateFields, token) {
				return "", nil, ErrInvalid
			}
			v, ok := fields[token]
			if !ok {
				if !slices.Contains(missing, token) {
					missing = append(missing, token)
				}
				continue
			}
			b.WriteString(v.Value)
		default:
			b.WriteByte(tmpl[i])
			i++
		}
	}
	return b.String(), missing, nil
}

func (s *Service) cardPreview(ctx context.Context, tx Tx, c GroupConfig, cmd Command) (Result, error) {
	out := Result{State: "unavailable", Action: "card_preview"}
	if !valid(cmd.Target, 2048) || !validText(cmd.Text, 1024) {
		return out, ErrInvalid
	}
	if s.Facts == nil {
		return out, ErrUnavailable
	}
	f, e := s.transactionFacts(ctx, tx, factKey{cmd.Target, c.School})
	if e != nil {
		return out, e
	}
	tmpl := c.CardTemplate
	if cmd.Text != "" {
		tmpl = cmd.Text
	}
	if tmpl == "" {
		return out, ErrDisabled
	}
	rendered, missing, e := renderCardTemplate(tmpl, f.Fields)
	if e != nil {
		return out, e
	}
	if len(missing) > 0 {
		return Result{State: "pending_profile", Action: "card_preview", Detail: strings.Join(missing, ","), MissingFields: missing}, nil
	}
	if runes := []rune(rendered); len(runes) > 2048 {
		rendered = string(runes[:2048])
	}
	return Result{State: "ready", Action: "card_preview", Detail: rendered}, nil
}

func (s *Service) card(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command) (Result, error) {
	out := Result{State: "unavailable"}
	if s.Facts == nil {
		return out, ErrUnavailable
	}
	f, e := s.transactionFacts(ctx, tx, factKey{cmd.Target, c.School})
	if e != nil {
		return out, e
	}
	if !c.CheckCard || c.CardTemplate == "" {
		return out, NewErrorDetail(ErrDisabled, "card_check_disabled", "check_card", 0)
	}
	text, missing, e := renderCardTemplate(c.CardTemplate, f.Fields)
	if e != nil {
		return out, e
	}
	if utf8.RuneCountInString(text) > 2048 {
		return out, ErrUnprocessable
	}
	if len(missing) > 0 {
		return Result{State: "pending_profile"}, nil
	}
	out.Detail = text
	if text == cmd.Text {
		out.State = "compliant"
		return out, nil
	}
	out.State = "noncompliant"
	if c.RemindCard {
		if e = s.auth(ctx, p, "groups.manage", c); e != nil {
			return out, e
		}
		if _, e = s.queue(ctx, tx, p, c, Command{Kind: "remind", Target: cmd.Target, Text: "card_noncompliant"}, "groups.manage"); e != nil {
			return out, e
		}
	}
	if !c.AutoCard {
		return out, nil
	}
	if e = s.auth(ctx, p, "groups.manage", c); e != nil {
		return out, e
	}
	return s.queue(ctx, tx, p, c, Command{Kind: "card", Target: cmd.Target, Text: text}, "groups.manage")
}

func (s *Service) cancelAction(ctx context.Context, tx Tx, g string, a *Action, reason string) error {
	a.State = "cancelled"
	a.LeaseUntil = nil
	if reason != "" {
		a.Result = reason
	}
	if err := s.applyMuteOutcome(tx, g, *a); err != nil {
		return err
	}
	if err := tx.Delete(bucket(g, "action_traces"), a.ID); err != nil {
		return err
	}
	if e := tx.Put(bucket(g, "actions"), a.ID, *a); e != nil {
		return e
	}
	if e := s.audit(tx, a.Actor.Principal(), g, a.Command.Kind, a.Command.Target, a.State); e != nil {
		return e
	}
	return s.event(ctx, tx, g, "action.result", map[string]string{"id": a.ID, "state": a.State})
}

func (s *Service) authorizePending(ctx context.Context, tx Tx, g string, a *Action) error {
	var c GroupConfig
	if e := tx.Get("config", g, &c); e != nil {
		if errors.Is(e, ErrNotFound) {
			return s.cancelAction(ctx, tx, g, a, e.Error())
		}
		return e
	}
	if a.Capability == "admission" {
		var m Member
		if e := tx.Get(bucket(g, "members"), a.Command.Target, &m); e != nil {
			return e
		}
		ok, e := s.eligible(ctx, tx, c, m)
		if e != nil {
			return e
		}
		cancel := (a.Command.Kind != "approve" && !m.Joined) || (a.Command.Kind == "approve" && m.Joined) || (a.Command.Kind == "kick" && (ok || m.Exempt || s.now().Before(m.Deadline))) || (a.Command.Kind == "remind" && (ok || m.Exempt)) || (a.Command.Kind == "approve" && !ok) || (a.Command.Kind == "welcome" && !ok) || (a.Command.Kind == "mute" && (ok || m.Exempt)) || (a.Command.Kind == "unmute" && !ok)
		if cancel {
			return s.cancelAction(ctx, tx, g, a, "")
		}
	} else if a.Capability == "entertainment" {
		var m Member
		if e := tx.Get(bucket(g, "members"), a.Command.Target, &m); e != nil {
			return e
		}
		ok, e := s.canPlay(ctx, tx, c, m)
		if e != nil {
			return e
		}
		if !ok || c.MuteProbability == 0 || a.Command.Target != a.Actor.Key() {
			return s.cancelAction(ctx, tx, g, a, "")
		}
	} else {
		if e := s.auth(ctx, a.Actor.Principal(), a.Capability, c); e != nil {
			if errors.Is(e, ErrForbidden) || errors.Is(e, ErrUnauthenticated) {
				return s.cancelAction(ctx, tx, g, a, e.Error())
			}
			return e
		}
	}
	if a.Command.Kind == "forward" {
		if err := s.checkForward(ctx, tx, a.Actor.Principal(), c, a.Command.Message); err != nil {
			if errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrNotFound) {
				return s.cancelAction(ctx, tx, g, a, err.Error())
			}
			return err
		}
	}
	if a.Command.Kind == "send" && a.Command.Receiver != "" {
		var source GroupConfig
		if e := tx.Get("config", a.Command.Receiver, &source); e != nil {
			if errors.Is(e, ErrNotFound) {
				return s.cancelAction(ctx, tx, g, a, ErrNotFound.Error())
			}
			return e
		}
		if e := s.auth(ctx, a.Actor.Principal(), "groups.read", source); e != nil {
			if errors.Is(e, ErrForbidden) || errors.Is(e, ErrUnauthenticated) {
				return s.cancelAction(ctx, tx, g, a, e.Error())
			}
			return e
		}
	}
	if !c.BotEnabled {
		return s.cancelAction(ctx, tx, g, a, ErrDisabled.Error())
	}
	return nil
}

// Execute serializes claims before an external call. Interrupted calls remain
// unknown and are resolved via Lookup on the next invocation with the same ID.
func (s *Service) Execute(ctx context.Context, g, id string) (Action, error) {
	var a Action
	var lookup bool
	e := s.runPrepared(ctx, []string{"group:" + g}, func(tx *preparedTx) error {
		if e := tx.Get(bucket(g, "actions"), id, &a); e != nil {
			return e
		}
		if TraceParentFromContext(ctx) == "" {
			var storedTP string
			if tx.Get(bucket(g, "action_traces"), id, &storedTP) == nil && validTraceParent(storedTP) {
				ctx = WithTraceParent(ctx, storedTP)
			}
		}
		if a.State == "succeeded" || a.State == "failed" || a.State == "cancelled" {
			return nil
		}
		if a.State == "executing" {
			return NewErrorDetail(ErrConflict, "action_executing", "", 0)
		}
		lookup = a.State == "unknown"
		if !lookup {
			if e := s.authorizePending(ctx, tx, g, &a); e != nil || a.State == "cancelled" {
				return e
			}
		}
		if s.Bot == nil {
			return ErrUnavailable
		}
		a.State = "executing"
		deadline := s.now().Add(30 * time.Second)
		a.LeaseUntil = &deadline
		a.Result = ""
		return tx.Put(bucket(g, "actions"), id, a)
	})
	if e != nil || a.State != "executing" {
		return a, e
	}
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var result Outcome
	found := false
	if lookup {
		result, found, e = s.Bot.Lookup(call, a.ID)
		if e == nil && !found {
			// The adapter confirmed that nothing happened. Recheck permission before retrying.
			e = s.runPrepared(call, []string{"group:" + g}, func(tx *preparedTx) error { return s.authorizePending(call, tx, g, &a) })
			if e == nil && a.State == "cancelled" {
				return a, nil
			}
		}
	}
	if e == nil && !found {
		result, e = s.Bot.Execute(call, a)
	}
	// A timeout is an unknown outcome. The adapter must resolve it, never replay blindly.
	a.State = "succeeded"
	a.LeaseUntil = nil
	a.Result = result.Code
	if !result.Success {
		a.State = "failed"
	}
	if e != nil {
		a.State = "unknown"
		a.Result = "lookup_required"
	}
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelFinish()
	err := s.runScoped(finish, []string{"group:" + g}, func(tx Tx) error {
		if a.State != "unknown" {
			if err := tx.Delete(bucket(g, "action_traces"), id); err != nil {
				return err
			}
		}
		if err := s.applyMuteOutcome(tx, g, a); err != nil {
			return err
		}
		if err := tx.Put(bucket(g, "actions"), id, a); err != nil {
			return err
		}
		if a.Command.Kind == "kick" && a.State == "succeeded" {
			var m Member
			if err := tx.Get(bucket(g, "members"), a.Command.Target, &m); err == nil {
				m.State = "removed"
				m.Joined = false
				if err = tx.Put(bucket(g, "members"), m.Owner, m); err != nil {
					return err
				}
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		if a.State == "succeeded" && a.Command.Kind == "warn" {
			var warnings Warnings
			err := tx.Get(bucket(g, "warnings"), a.Command.Target, &warnings)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			warnings.Count++
			if err = tx.Put(bucket(g, "warnings"), a.Command.Target, warnings); err != nil {
				return err
			}
			var c GroupConfig
			if err = tx.Get("config", g, &c); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if err == nil && c.WarningLimit > 0 && c.MuteSeconds > 0 && warnings.Count%c.WarningLimit == 0 && slices.Contains(c.AutoActions, "mute") {
				if authErr := s.auth(finish, a.Actor.Principal(), "groups.manage", c); authErr != nil {
					if !errors.Is(authErr, ErrForbidden) && !errors.Is(authErr, ErrUnauthenticated) {
						return authErr
					}
					if err = s.audit(tx, a.Actor.Principal(), g, "mute", a.Command.Target, "cancelled"); err != nil {
						return err
					}
				} else if _, qErr := s.queue(finish, tx, a.Actor.Principal(), c, Command{Kind: "mute", Target: a.Command.Target, Seconds: c.MuteSeconds, Text: "warning_limit"}, "groups.manage"); qErr != nil {
					if !errors.Is(qErr, ErrDisabled) && !errors.Is(qErr, ErrCooldown) {
						return qErr
					}
					if err = s.audit(tx, a.Actor.Principal(), g, "mute", a.Command.Target, "skipped"); err != nil {
						return err
					}
				}
			}
		}
		if err := s.audit(tx, a.Actor.Principal(), g, a.Command.Kind, a.Command.Target, a.State); err != nil {
			return err
		}
		return s.event(finish, tx, g, "action.result", map[string]string{"id": id, "state": a.State})
	})
	if err != nil {
		return a, err
	}
	return a, e
}

// Recover is called by the host for expired execution leases, with bounded pages.
func (s *Service) Recover(ctx context.Context, g, cursor string) (string, error) {
	next := ""
	e := s.runScoped(ctx, []string{"group:" + g}, func(tx Tx) error {
		rows, e := tx.List(bucket(g, "actions"), cursor, 100)
		if e != nil {
			return e
		}
		for _, r := range rows {
			next = r.ID
			var a Action
			if e = json.Unmarshal(r.Data, &a); e != nil {
				return e
			}
			if a.State == "executing" && a.LeaseUntil != nil && !s.now().Before(*a.LeaseUntil) {
				a.LeaseUntil = nil
				a.State = "unknown"
				a.Result = "lookup_required"
				if e = tx.Put(bucket(g, "actions"), r.ID, a); e != nil {
					return e
				}
			}
		}
		if len(rows) < 100 {
			next = ""
		}
		return nil
	})
	if e != nil {
		return "", e
	}
	return next, nil
}
func (s *Service) manageAction(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command) (Result, error) {
	var out Result
	if !valid(cmd.Target, 2048) || !validText(cmd.Text, 2048) {
		return out, ErrInvalid
	}
	var a Action
	if e := tx.Get(bucket(c.ID, "actions"), cmd.Target, &a); e != nil {
		return out, e
	}
	if cmd.Kind == "cancel_action" {
		// Only pending actions may be cancelled; executing/unknown actions may already be in flight.
		if a.State != "pending" {
			return out, ErrConflict
		}
		if err := s.cancelAction(ctx, tx, c.ID, &a, "cancelled_by_admin"); err != nil {
			return out, err
		}
		if err := s.audit(tx, p, c.ID, "cancel_action", a.ID, "cancelled"); err != nil {
			return out, err
		}
		return Result{ID: a.ID, State: "cancelled", Action: a.Command.Kind, Detail: "cancelled_by_admin"}, nil
	}
	// retry_action only accepts confirmed failed actions not already retried.
	if a.State != "failed" || strings.HasPrefix(a.Result, "retried:") {
		return out, ErrConflict
	}
	if a.Capability != "" && a.Capability != "admission" && a.Capability != "entertainment" {
		if e := s.auth(ctx, p, a.Capability, c); e != nil {
			return out, e
		}
	}
	actor := p
	if a.Command.Kind == "forward" {
		if err := s.checkForward(ctx, tx, p, c, a.Command.Message); err != nil {
			return out, err
		}
	}
	if a.Capability == "entertainment" {
		actor = a.Actor.Principal()
		if !actor.Valid() || a.Command.Kind != "mute" || actor.Key() != a.Command.Target {
			return out, ErrConflict
		}
		var member Member
		if err := tx.Get(bucket(c.ID, "members"), actor.Key(), &member); err != nil {
			return out, err
		}
		if member.LastPlay.After(a.Created) || member.MutedUntil.After(s.now()) {
			return out, ErrConflict
		}
		var active string
		if err := tx.Get(bucket(c.ID, "self_mutes"), actor.Key(), &active); err == nil {
			return out, ErrConflict
		} else if !errors.Is(err, ErrNotFound) {
			return out, err
		}
	}
	queued, e := s.queue(ctx, tx, actor, c, a.Command, a.Capability)
	if e != nil {
		return out, e
	}
	if a.Capability == "entertainment" {
		if err := tx.Put(bucket(c.ID, "self_mutes"), actor.Key(), queued.ID); err != nil {
			return out, err
		}
	}
	a.Result = "retried:" + queued.ID
	if e := tx.Put(bucket(c.ID, "actions"), a.ID, a); e != nil {
		return out, e
	}
	if e := s.audit(tx, p, c.ID, "retry_action", a.ID, queued.ID); e != nil {
		return out, e
	}
	queued.Detail = a.ID
	return queued, nil
}

func (s *Service) Cleanup(ctx context.Context, g, cursor string) (string, error) {
	next := ""
	e := s.runScoped(ctx, []string{"group:" + g}, func(tx Tx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		rows, e := tx.List(bucket(g, "messages"), cursor, 100)
		if e != nil {
			return e
		}
		for _, r := range rows {
			next = r.ID
			var m Message
			if e = json.Unmarshal(r.Data, &m); e != nil {
				return e
			}
			if c.RetentionSeconds == 0 || !s.now().Before(m.Expires) {
				if e = tx.Delete(bucket(g, "messages"), r.ID); e != nil {
					return e
				}
			}
		}
		if len(rows) < 100 {
			next = ""
		}
		return nil
	})
	if e != nil {
		return "", e
	}
	return next, nil
}

func (s *Service) applyMuteOutcome(tx Tx, g string, a Action) error {
	self := a.Capability == "entertainment" && a.Command.Kind == "mute"
	unmuted := a.Command.Kind == "unmute" && a.State == "succeeded"
	if (!self && !unmuted) || a.State == "unknown" {
		return nil
	}
	var m Member
	err := tx.Get(bucket(g, "members"), a.Command.Target, &m)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if self {
		var id string
		err := tx.Get(bucket(g, "self_mutes"), m.Owner, &id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil && id != a.ID {
			return nil
		}
		if errors.Is(err, ErrNotFound) && m.LastPlay.After(a.Created) {
			return nil
		}
		if err := tx.Delete(bucket(g, "self_mutes"), m.Owner); err != nil {
			return err
		}
	}
	m.MutedUntil = time.Time{}
	if self && a.State == "succeeded" {
		m.MutedUntil = s.now().Add(time.Duration(a.Command.Seconds) * time.Second)
	}
	return tx.Put(bucket(g, "members"), m.Owner, m)
}

// CleanupIdempotency removes bounded pages of expired idempotency receipts or batch_inputs (default 24h).
func (s *Service) CleanupIdempotency(ctx context.Context, bucketName, cursor string, maxAge time.Duration) (string, error) {
	if bucketName != "receipts" && bucketName != "batch_inputs" {
		return "", ErrInvalid
	}
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	next := ""
	e := s.run(ctx, func(tx Tx) error {
		rows, e := tx.List(bucketName, cursor, 100)
		if e != nil {
			return e
		}
		for _, r := range rows {
			next = r.ID
			var meta struct {
				CreatedAt time.Time `json:"created_at"`
			}
			if json.Unmarshal(r.Data, &meta) == nil && !meta.CreatedAt.IsZero() && !s.now().Before(meta.CreatedAt.Add(maxAge)) {
				if e = tx.Delete(bucketName, r.ID); e != nil {
					return e
				}
			}
		}
		if len(rows) < 100 {
			next = ""
		}
		return nil
	})
	if e != nil {
		return "", e
	}
	return next, nil
}

// CleanupOutbox deletes at most 100 confirmed historical outbox rows older than maxAge (default 24h).
// Unsent or held outbox rows are never deleted.
func (s *Service) CleanupOutbox(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	deleted := 0
	e := s.run(ctx, func(tx Tx) error {
		var err error
		deleted, err = tx.DeleteSentOutboxBefore(s.now().Add(-maxAge), 100)
		return err
	})
	if e != nil {
		return 0, e
	}
	return deleted, nil
}

// CleanupFrequency removes bounded pages of expired message rate-limit windows for group g.
func (s *Service) CleanupFrequency(ctx context.Context, g, cursor string) (string, error) {
	next := ""
	e := s.run(ctx, func(tx Tx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		ttl := time.Duration(c.FrequencySeconds) * time.Second
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		rows, e := tx.List(bucket(g, "frequency"), cursor, 100)
		if e != nil {
			return e
		}
		for _, r := range rows {
			next = r.ID
			var window struct {
				Start time.Time
			}
			if e = json.Unmarshal(r.Data, &window); e != nil {
				return e
			}
			if c.FrequencyLimit == 0 || c.FrequencySeconds == 0 || !s.now().Before(window.Start.Add(ttl)) {
				if e = tx.Delete(bucket(g, "frequency"), r.ID); e != nil {
					return e
				}
			}
		}
		if len(rows) < 100 {
			next = ""
		}
		return nil
	})
	if e != nil {
		return "", e
	}
	return next, nil
}

func (s *Service) checkForward(ctx context.Context, tx ReadTx, actor account.Principal, c GroupConfig, id string) error {
	if err := s.auth(ctx, actor, "groups.messages.read", c); err != nil {
		return err
	}
	var msg Message
	if err := tx.Get(bucket(c.ID, "messages"), id, &msg); err != nil {
		return err
	}
	if c.RetentionSeconds == 0 || !s.now().Before(msg.Expires) {
		return ErrNotFound
	}
	return nil
}
