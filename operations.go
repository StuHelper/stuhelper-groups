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
	if !valid(g, 128) || event.Source != "qq-bot" || (event.Subject != g && event.Subject != stream) || event.Type != "groups.message" || !valid(m.ID, 128) || !valid(m.Owner, 2048) || !validText(m.Body, 8192) {
		return ErrInvalid
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
	return s.run(ctx, func(tx Tx) error {
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
		return s.event(tx, g, "message.changed", map[string]string{"id": m.ID})
	})
}

func (s *Service) rule(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command, preview bool) (Result, error) {
	out := Result{State: "clear"}
	hits := []string{}
	if c.BlockLinks && (strings.Contains(cmd.Text, "http://") || strings.Contains(cmd.Text, "https://")) {
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
			if _, e = s.queue(tx, p, c, command, cap); e != nil {
				return out, e
			}
		}
	}
	return out, nil
}

// A nil field map validates the template without expanding member data.
func renderCard(template string, fields map[string]CardField) (string, bool, error) {
	var out strings.Builder
	complete := true
	for len(template) > 0 {
		i := strings.IndexAny(template, "{}")
		if i < 0 {
			out.WriteString(template)
			break
		}
		out.WriteString(template[:i])
		if template[i] != '{' {
			return "", false, ErrInvalid
		}
		token, rest, ok := strings.Cut(template[i+1:], "}")
		if !ok {
			return "", false, ErrInvalid
		}
		template = rest
		switch token {
		case "separator":
			switch fields["gender"].Value {
			case "male":
				out.WriteString("-")
			case "female":
				out.WriteString("+")
			default:
				out.WriteString("·")
			}
		case "school", "grade", "department", "name", "student_id", "gender", "nickname":
			field, ok := fields[token]
			if !ok {
				complete = false
			}
			out.WriteString(field.Value)
		default:
			return "", false, ErrInvalid
		}
	}
	if utf8.RuneCountInString(out.String()) > 2048 {
		return "", false, ErrUnprocessable
	}
	return out.String(), complete, nil
}
func (s *Service) card(ctx context.Context, tx Tx, p account.Principal, c GroupConfig, cmd Command) (Result, error) {
	out := Result{State: "unavailable"}
	if s.Facts == nil {
		return out, ErrUnavailable
	}
	f, e := s.currentFacts(ctx, cmd.Target, c.School)
	if e != nil {
		return out, ErrUnavailable
	}
	if !c.CheckCard || c.CardTemplate == "" {
		return out, ErrDisabled
	}
	text, complete, err := renderCard(c.CardTemplate, f.Fields)
	if err != nil {
		return out, err
	}
	if !complete {
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
		if _, e = s.queue(tx, p, c, Command{Kind: "remind", Target: cmd.Target, Text: "card_noncompliant"}, "groups.manage"); e != nil {
			return out, e
		}
	}
	if !c.AutoCard {
		return out, nil
	}
	if e = s.auth(ctx, p, "groups.manage", c); e != nil {
		return out, e
	}
	return s.queue(tx, p, c, Command{Kind: "card", Target: cmd.Target, Text: text}, "groups.manage")
}

func (s *Service) cancelAction(tx Tx, g string, a *Action, reason string) error {
	a.State = "cancelled"
	a.LeaseUntil = nil
	if reason != "" {
		a.Result = reason
	}
	if e := tx.Put(bucket(g, "actions"), a.ID, *a); e != nil {
		return e
	}
	if e := s.audit(tx, a.Actor.Principal(), g, a.Command.Kind, a.Command.Target, a.State); e != nil {
		return e
	}
	return s.event(tx, g, "action.result", map[string]string{"id": a.ID, "state": a.State})
}

func (s *Service) authorizePending(ctx context.Context, tx Tx, g string, a *Action) error {
	var c GroupConfig
	if e := tx.Get("config", g, &c); e != nil {
		if errors.Is(e, ErrNotFound) {
			return s.cancelAction(tx, g, a, e.Error())
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
			return s.cancelAction(tx, g, a, "")
		}
	} else if a.Capability == "entertainment" {
		var m Member
		if e := tx.Get(bucket(g, "members"), a.Command.Target, &m); e != nil {
			return e
		}
		ok, e := s.eligible(ctx, tx, c, m)
		if e != nil {
			return e
		}
		if !m.Joined || !ok || c.MuteProbability == 0 || a.Command.Target != a.Actor.Key() {
			return s.cancelAction(tx, g, a, "")
		}
	} else {
		if e := s.auth(ctx, a.Actor.Principal(), a.Capability, c); e != nil {
			if errors.Is(e, ErrForbidden) || errors.Is(e, ErrUnauthenticated) {
				return s.cancelAction(tx, g, a, e.Error())
			}
			return e
		}
	}
	if a.Command.Kind == "send" && a.Command.Receiver != "" {
		var source GroupConfig
		if e := tx.Get("config", a.Command.Receiver, &source); e != nil {
			if errors.Is(e, ErrNotFound) {
				return s.cancelAction(tx, g, a, ErrNotFound.Error())
			}
			return e
		}
		if e := s.auth(ctx, a.Actor.Principal(), "groups.read", source); e != nil {
			if errors.Is(e, ErrForbidden) || errors.Is(e, ErrUnauthenticated) {
				return s.cancelAction(tx, g, a, e.Error())
			}
			return e
		}
	}
	if !c.BotEnabled {
		return s.cancelAction(tx, g, a, ErrDisabled.Error())
	}
	return nil
}

// Execute serializes claims before an external call. Interrupted calls remain
// unknown and are resolved via Lookup on the next invocation with the same ID.
func (s *Service) Execute(ctx context.Context, g, id string) (Action, error) {
	var a Action
	var lookup bool
	e := s.run(ctx, func(tx Tx) error {
		if e := tx.Get(bucket(g, "actions"), id, &a); e != nil {
			return e
		}
		if a.State == "succeeded" || a.State == "failed" || a.State == "cancelled" {
			return nil
		}
		if a.State == "executing" {
			return ErrConflict
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
			e = s.run(call, func(tx Tx) error { return s.authorizePending(call, tx, g, &a) })
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
	err := s.run(finish, func(tx Tx) error {
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
				} else if _, qErr := s.queue(tx, a.Actor.Principal(), c, Command{Kind: "mute", Target: a.Command.Target, Seconds: c.MuteSeconds, Text: "warning_limit"}, "groups.manage"); qErr != nil {
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
		return s.event(tx, g, "action.result", map[string]string{"id": id, "state": a.State})
	})
	if err != nil {
		return a, err
	}
	return a, e
}

// Recover is called by the host for expired execution leases, with bounded pages.
func (s *Service) Recover(ctx context.Context, g, cursor string) (string, error) {
	next := ""
	e := s.run(ctx, func(tx Tx) error {
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
	return next, e
}
func (s *Service) Cleanup(ctx context.Context, g, cursor string) (string, error) {
	next := ""
	e := s.run(ctx, func(tx Tx) error {
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
	return next, e
}
