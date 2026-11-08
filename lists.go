package groups

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/StuHelper/stuhelper-platform/account"
)

type Report struct {
	ID          string   `json:"id"`
	Reporter    string   `json:"reporter"`
	Message     string   `json:"message"`
	Reason      string   `json:"reason"`
	State       string   `json:"state"`
	Analysis    string   `json:"analysis,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`
	Feedback    string   `json:"feedback,omitempty"`
}
type Hit struct {
	ID      string   `json:"id"`
	Owner   string   `json:"owner"`
	Message string   `json:"message"`
	Rules   []string `json:"rules"`
	State   string   `json:"state"`
}
type Subscription struct {
	ID       string `json:"id"`
	Receiver string `json:"receiver"`
	Enabled  bool   `json:"enabled"`
	Actor    Actor  `json:"actor"`
}
type Warnings struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
}
type Watch struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}
type Delivery struct {
	ID       string `json:"id"`
	Receiver string `json:"receiver"`
	Action   string `json:"action"`
	State    string `json:"state"`
}

// Pages use the last scanned ID, so authorization/retention filtering may yield
// an empty page with a nonempty cursor. Each request examines at most limit rows.
func listRecords[T any](s *Service, ctx context.Context, p account.Principal, g, collection, cursor string, limit int) (Page[T], error) {
	out := Page[T]{Items: []T{}}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if limit < 1 || limit > 100 || len(cursor) > 4096 {
		return out, ErrInvalid
	}
	e := s.run(ctx, func(tx Tx) error {
		var c GroupConfig
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
			var record map[string]json.RawMessage
			if e = json.Unmarshal(r.Data, &record); e != nil {
				return e
			}
			record["id"], e = json.Marshal(r.ID)
			if e != nil {
				return e
			}
			if collection == "deliveries" {
				var d Delivery
				if e = json.Unmarshal(r.Data, &d); e != nil {
					return e
				}
				var action Action
				if e = tx.Get(bucket(d.Receiver, "actions"), d.Action, &action); e != nil {
					return e
				}
				record["state"], e = json.Marshal(action.State)
				if e != nil {
					return e
				}
			}
			data, e := json.Marshal(record)
			if e != nil {
				return e
			}
			var item T
			if e = json.Unmarshal(data, &item); e != nil {
				return e
			}
			out.Items = append(out.Items, item)
		}
		if len(rows) < limit {
			out.NextCursor = ""
		}
		return nil
	})
	if e != nil {
		return Page[T]{Items: []T{}}, e
	}
	return out, nil
}

func (s *Service) Members(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Member], error) {
	return listRecords[Member](s, ctx, p, g, "members", cursor, limit)
}

func (s *Service) Actions(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Action], error) {
	return listRecords[Action](s, ctx, p, g, "actions", cursor, limit)
}

func (s *Service) Reports(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Report], error) {
	return listRecords[Report](s, ctx, p, g, "reports", cursor, limit)
}

func (s *Service) Hits(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Hit], error) {
	return listRecords[Hit](s, ctx, p, g, "hits", cursor, limit)
}

func (s *Service) Messages(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Message], error) {
	return listRecords[Message](s, ctx, p, g, "messages", cursor, limit)
}

func (s *Service) Subscriptions(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Subscription], error) {
	return listRecords[Subscription](s, ctx, p, g, "subscriptions", cursor, limit)
}

func (s *Service) Blacklist(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Blacklist], error) {
	return listRecords[Blacklist](s, ctx, p, g, "blacklist", cursor, limit)
}

func (s *Service) Warnings(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Warnings], error) {
	return listRecords[Warnings](s, ctx, p, g, "warnings", cursor, limit)
}

func (s *Service) Watch(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Watch], error) {
	return listRecords[Watch](s, ctx, p, g, "watch", cursor, limit)
}

func (s *Service) Deliveries(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Delivery], error) {
	return listRecords[Delivery](s, ctx, p, g, "deliveries", cursor, limit)
}

func (s *Service) Groups(ctx context.Context, p account.Principal, cursor string, limit int) (Page[GroupSummary], error) {
	out := Page[GroupSummary]{Items: []GroupSummary{}}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if limit < 1 || limit > 100 || len(cursor) > 4096 {
		return out, ErrInvalid
	}
	e := s.run(ctx, func(tx Tx) error {
		rows, e := tx.List("config", cursor, limit)
		if e != nil {
			return e
		}
		ids := []string{}
		for _, r := range rows {
			out.NextCursor = r.ID
			var c GroupConfig
			if e = json.Unmarshal(r.Data, &c); e != nil {
				return e
			}
			if e = s.auth(ctx, p, "groups.read", c); e != nil {
				if errors.Is(e, ErrForbidden) {
					continue
				}
				return e
			}
			out.Items = append(out.Items, GroupSummary{ID: c.ID, School: c.School, Name: c.Name, Number: c.Number, BotEnabled: c.BotEnabled, RequireStudent: c.RequireStudent, Revision: c.Revision})
			ids = append(ids, c.ID)
		}
		counts, e := tx.GroupCounts(ids)
		if e != nil {
			return e
		}
		for i := range out.Items {
			n := counts[out.Items[i].ID]
			out.Items[i].MemberCount = n.Members
			out.Items[i].PendingCount = n.Pending
		}
		if len(rows) < limit {
			out.NextCursor = ""
		}
		return nil
	})
	if e != nil {
		return Page[GroupSummary]{Items: []GroupSummary{}}, e
	}
	return out, nil
}
