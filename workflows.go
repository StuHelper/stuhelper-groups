package groups

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/StuHelper/stuhelper-platform/account"
	"slices"
	"time"
)

// Sweep is a bounded host job for qualification deadlines and reminders. Host
// persists its cursor and periodically starts at the beginning for new deadlines.
func (s *Service) Sweep(ctx context.Context, g, cursor string) (string, error) {
	var school string
	owners := []string{}
	e := s.runRead(ctx, func(tx ReadTx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		school = c.School
		rows, e := tx.List(bucket(g, "members"), cursor, 100)
		if e != nil {
			return e
		}
		for _, r := range rows {
			var m Member
			if e = json.Unmarshal(r.Data, &m); e != nil {
				return e
			}
			if m.State == "removed" {
				continue
			}
			restricted, e := isRestrictedByBlacklist(tx, c.ID, m.Owner, s.now())
			if e != nil {
				return e
			}
			if !restricted {
				owners = append(owners, m.Owner)
			}
		}
		return nil
	})
	if e != nil {
		return "", e
	}
	cache := map[factKey]Facts{}
	for _, owner := range owners {
		key := factKey{owner, school}
		if _, ok := cache[key]; !ok {
			facts, err := s.currentFacts(ctx, owner, school)
			if err != nil {
				return "", ErrUnavailable
			}
			cache[key] = facts
		}
	}
	ctx = withFactCache(ctx, cache)
	next := ""
	e = s.runScoped(ctx, []string{"group:" + g}, func(tx Tx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		rows, e := tx.List(bucket(g, "members"), cursor, 100)
		if e != nil {
			return e
		}
		for _, r := range rows {
			next = r.ID
			var m Member
			if e = json.Unmarshal(r.Data, &m); e != nil {
				return e
			}
			if m.State == "removed" {
				continue
			}
			if _, e = s.admission(ctx, tx, account.Principal{}, c, m.Owner, false); e != nil {
				return e
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

// Fanout consumes committed disposition events, independently queuing each
// enabled recipient. Delivery state is the recipient action's actual state.
func (s *Service) Fanout(ctx context.Context, e Event) error {
	if e.Source != "groups" || e.Type != "groups.action.result" {
		return ErrInvalid
	}
	scopes := []string{"group:" + e.Subject}
	preErr := s.runRead(ctx, func(tx ReadTx) error {
		rows, err := tx.List(bucket(e.Subject, "subscriptions"), "", 100)
		if err != nil {
			return err
		}
		for _, r := range rows {
			var sub struct {
				Receiver string `json:"receiver"`
				Enabled  bool   `json:"enabled"`
			}
			if err = json.Unmarshal(r.Data, &sub); err != nil {
				return err
			}
			if sub.Enabled && sub.Receiver != "" {
				scopes = append(scopes, "group:"+sub.Receiver)
			}
		}
		return nil
	})
	if preErr != nil {
		return preErr
	}
	return s.runScoped(ctx, scopes, func(tx Tx) error {
		copy := e
		copy.Source = "groups-subscriptions"
		ok, err := tx.Accept(copy)
		if err != nil || !ok {
			return err
		}
		var data struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(e.Data, &data); err != nil {
			return ErrInvalid
		}
		var origin Action
		if err = tx.Get(bucket(e.Subject, "actions"), data.ID, &origin); err != nil {
			return err
		}
		if !slices.Contains([]string{"warn", "mute", "unmute", "kick", "recall"}, origin.Command.Kind) {
			return nil
		}
		rows, err := tx.List(bucket(e.Subject, "subscriptions"), "", 100)
		if err != nil {
			return err
		}
		for _, r := range rows {
			var sub struct {
				Receiver string `json:"receiver"`
				Enabled  bool   `json:"enabled"`
				Actor    Actor  `json:"actor"`
			}
			if err = json.Unmarshal(r.Data, &sub); err != nil {
				return err
			}
			if !sub.Enabled {
				continue
			}
			var c GroupConfig
			if err = tx.Get("config", sub.Receiver, &c); err != nil {
				return err
			}
			result, err := s.queue(tx, sub.Actor.Principal(), c, Command{Kind: "send", Receiver: e.Subject, Text: "group_event:" + origin.ID + ":" + origin.State}, "groups.messages.send")
			if err != nil {
				return err
			}
			if err = tx.Put(bucket(e.Subject, "deliveries"), e.ID+"/"+r.ID, map[string]string{"receiver": sub.Receiver, "action": result.ID}); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Service) frequency(tx Tx, c GroupConfig, cmd Command, preview bool) (bool, error) {
	if c.FrequencyLimit == 0 || c.FrequencySeconds == 0 {
		return false, nil
	}
	var window struct {
		Start time.Time
		Count int64
	}
	e := tx.Get(bucket(c.ID, "frequency"), cmd.Target, &window)
	if e != nil && !errors.Is(e, ErrNotFound) {
		return false, e
	}
	if !s.now().Before(window.Start.Add(time.Duration(c.FrequencySeconds) * time.Second)) {
		window.Start = s.now()
		window.Count = 0
	}
	window.Count++
	if !preview {
		if e = tx.Put(bucket(c.ID, "frequency"), cmd.Target, window); e != nil {
			return false, e
		}
	}
	return window.Count > c.FrequencyLimit, nil
}

// MemberEvent receives verified group membership from the host bot adapter.
// A web application alone cannot claim that a QQ user has already joined.
func (s *Service) MemberEvent(ctx context.Context, g string, event Event, owner string) error {
	if event.SpecVersion != "1.0" || event.ID == "" || event.Revision < 1 || event.Time.IsZero() || event.Source != "qq-bot" || event.Subject != g || !valid(owner, 2048) || !slices.Contains([]string{"groups.member_joined", "groups.member_left"}, event.Type) {
		return ErrInvalid
	}
	if event.Type == "groups.member_joined" {
		var school string
		var needFacts bool
		preErr := s.runRead(ctx, func(tx ReadTx) error {
			var c GroupConfig
			if e := tx.Get("config", g, &c); e != nil {
				return e
			}
			school = c.School
			restricted, e := isRestrictedByBlacklist(tx, c.ID, owner, s.now())
			if e != nil {
				return e
			}
			needFacts = !restricted
			return nil
		})
		if preErr != nil {
			return preErr
		}
		if needFacts {
			facts, e := s.currentFacts(ctx, owner, school)
			if e != nil {
				return ErrUnavailable
			}
			ctx = withFactCache(ctx, map[factKey]Facts{{owner, school}: facts})
		}
	}
	return s.runScoped(ctx, []string{"group:" + g}, func(tx Tx) error {
		payload, e := json.Marshal(struct {
			Owner string
			Data  json.RawMessage
		}{owner, event.Data})
		if e != nil {
			return e
		}
		event.Data = payload
		ok, e := tx.Accept(event)
		if e != nil || !ok {
			return e
		}
		var c GroupConfig
		if e = tx.Get("config", g, &c); e != nil {
			return e
		}
		if event.Type == "groups.member_joined" {
			if _, e = s.admission(ctx, tx, account.Principal{}, c, owner, true); e != nil {
				return e
			}
		}
		var m Member
		if e = tx.Get(bucket(g, "members"), owner, &m); e != nil {
			return e
		}
		m.Joined = event.Type == "groups.member_joined"
		if !m.Joined {
			m.State = "removed"
		}
		return tx.Put(bucket(g, "members"), owner, m)
	})
}
