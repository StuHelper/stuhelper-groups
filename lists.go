package groups

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/StuHelper/stuhelper-platform/account"
)

type Report struct {
	ID          string     `json:"id"`
	Reporter    string     `json:"reporter"`
	Target      string     `json:"target,omitempty"`
	Message     string     `json:"message"`
	Reason      string     `json:"reason"`
	State       string     `json:"state"`
	Analysis    string     `json:"analysis,omitempty"`
	Suggestions []string   `json:"suggestions,omitempty"`
	Feedback    string     `json:"feedback,omitempty"`
	ResolvedBy  *Actor     `json:"resolved_by,omitempty"`
	Created     *time.Time `json:"created,omitempty"`
	Updated     *time.Time `json:"updated,omitempty"`
}
type Hit struct {
	ID      string     `json:"id"`
	Owner   string     `json:"owner"`
	Message string     `json:"message"`
	Rules   []string   `json:"rules"`
	State   string     `json:"state"`
	Created *time.Time `json:"created,omitempty"`
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
	ID      string     `json:"id"`
	Enabled bool       `json:"enabled"`
	Reason  string     `json:"reason"`
	Created *time.Time `json:"created,omitempty"`
	Updated *time.Time `json:"updated,omitempty"`
}
type Delivery struct {
	ID       string `json:"id"`
	Receiver string `json:"receiver"`
	Action   string `json:"action"`
	State    string `json:"state"`
}
type AuditEntry struct {
	ID     string    `json:"id"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Result string    `json:"result"`
	Time   time.Time `json:"time"`
}

func (x *AuditEntry) setID(id string) { x.ID = id }

type idSetter interface {
	setID(string)
}

func (x *Member) setID(id string)       { x.ID = id }
func (x *Action) setID(id string)       { x.ID = id }
func (x *Report) setID(id string)       { x.ID = id }
func (x *Hit) setID(id string)          { x.ID = id }
func (x *Message) setID(id string)      { x.ID = id }
func (x *Subscription) setID(id string) { x.ID = id }
func (x *Blacklist) setID(id string)    { x.ID = id }
func (x *Warnings) setID(id string)     { x.ID = id }
func (x *Watch) setID(id string)        { x.ID = id }
func (x *Delivery) setID(id string)     { x.ID = id }

type MemberQuery struct {
	Cursor string
	Limit  int
	State  string
	Joined *bool
	Exempt *bool
}

type ActionQuery struct {
	Cursor string
	Limit  int
	Order  string
	State  string
	Kind   string
}

type ReportQuery struct {
	Cursor string
	Limit  int
	Order  string
	State  string
}

type HitQuery struct {
	Cursor string
	Limit  int
	Order  string
}

type MessageQuery struct {
	Cursor   string
	Limit    int
	Order    string
	Recalled *bool
}

const descCursorTimeLayout = "2006-01-02T15:04:05.000000Z"

type descCursorPayload struct {
	Time string `json:"t"`
	ID   string `json:"id"`
}

func encodeDescCursor(at time.Time, id string) string {
	b, _ := json.Marshal(descCursorPayload{Time: at.UTC().Truncate(time.Microsecond).Format(descCursorTimeLayout), ID: id})
	return "/d1/" + base64.RawURLEncoding.EncodeToString(b)
}

func decodeDescCursor(raw string) (time.Time, string, error) {
	encoded, ok := strings.CutPrefix(raw, "/d1/")
	if !ok {
		encoded, ok = strings.CutPrefix(raw, "d1:")
	}
	if !ok || encoded == "" {
		return time.Time{}, "", ErrInvalid
	}
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return time.Time{}, "", ErrInvalid
	}
	var payload descCursorPayload
	if err := json.Unmarshal(b, &payload); err != nil {
		return time.Time{}, "", ErrInvalid
	}
	if payload.ID == "" || !utf8.ValidString(payload.ID) || strings.ContainsFunc(payload.ID, unicode.IsControl) {
		return time.Time{}, "", ErrInvalid
	}
	at, err := time.Parse(descCursorTimeLayout, payload.Time)
	if err != nil {
		return time.Time{}, "", ErrInvalid
	}
	return at.UTC(), payload.ID, nil
}

func parseOrder(order string) (bool, error) {
	switch order {
	case "", "asc":
		return false, nil
	case "desc":
		return true, nil
	default:
		return false, ErrInvalid
	}
}

var validActionKinds = []string{
	"ai_execute", "analyze", "apply", "approve", "blacklist", "card", "dice", "exempt", "extend",
	"feedback", "forward", "kick", "mute", "password", "preview", "recall", "recheck", "remind",
	"report", "rule", "self_mute", "send", "subscribe", "unblacklist", "unmute", "warn", "watch", "welcome",
}

// Pages use the last scanned cursor, so authorization/retention filtering may yield
// an empty page with a nonempty cursor. Each request examines at most limit rows.
func listRecords[T any](s *Service, ctx context.Context, p account.Principal, g, collection string, q ListQuery) (Page[T], error) {
	out := Page[T]{Items: []T{}}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !valid(g, 128) || q.Limit < 1 || q.Limit > 100 || len(q.Cursor) > 4096 || !utf8.ValidString(q.Cursor) || strings.ContainsFunc(q.Cursor, unicode.IsControl) {
		return out, ErrInvalid
	}
	if q.Desc {
		if q.Cursor != "" {
			curTime, curID, err := decodeDescCursor(q.Cursor)
			if err != nil {
				return out, ErrInvalid
			}
			q.CursorTime = &curTime
			q.Cursor = curID
		}
	} else if slices.Contains([]string{"actions", "reports", "hits", "messages"}, collection) && strings.HasPrefix(q.Cursor, "/d1/") {
		return out, ErrInvalid
	}
	e := s.runRead(ctx, func(tx ReadTx) error {
		var c GroupConfig
		if e := tx.Get("config", g, &c); e != nil {
			return e
		}
		cap := "groups.read"
		if collection == "messages" {
			cap = "groups.messages.read"
		} else if collection == "audit" {
			cap = "groups.configure"
		}
		if e := s.auth(ctx, p, cap, c); e != nil {
			return e
		}
		rows, e := tx.QueryList(bucket(g, collection), q)
		if e != nil {
			return e
		}
		for _, r := range rows {
			if q.Desc {
				out.NextCursor = encodeDescCursor(r.CreatedAt, r.ID)
			} else {
				out.NextCursor = r.ID
			}
			var item T
			if e = json.Unmarshal(r.Data, &item); e != nil {
				return e
			}
			if setter, ok := any(&item).(idSetter); ok {
				setter.setID(r.ID)
			}
			switch v := any(&item).(type) {
			case *Message:
				if c.RetentionSeconds == 0 || !s.now().Before(v.Expires) {
					continue
				}
			case *Delivery:
				if v.Action != "" {
					var action Action
					if err := tx.Get(bucket(v.Receiver, "actions"), v.Action, &action); err == nil {
						v.State = action.State
					} else if !errors.Is(err, ErrNotFound) || v.State == "" {
						return err
					}
				}
			}
			out.Items = append(out.Items, item)
		}
		if len(rows) < q.Limit {
			out.NextCursor = ""
		}
		return nil
	})
	if e != nil {
		return Page[T]{Items: []T{}}, e
	}
	return out, nil
}

func (s *Service) QueryMembers(ctx context.Context, p account.Principal, g string, q MemberQuery) (Page[Member], error) {
	if q.State != "" && !slices.Contains([]string{"requested", "awaiting_verification", "grace", "extended", "exempt", "restored", "removal_pending", "removed"}, q.State) {
		return Page[Member]{Items: []Member{}}, ErrInvalid
	}
	return listRecords[Member](s, ctx, p, g, "members", ListQuery{Cursor: q.Cursor, Limit: q.Limit, State: q.State, Joined: q.Joined, Exempt: q.Exempt})
}

func (s *Service) Members(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Member], error) {
	return s.QueryMembers(ctx, p, g, MemberQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) QueryActions(ctx context.Context, p account.Principal, g string, q ActionQuery) (Page[Action], error) {
	desc, err := parseOrder(q.Order)
	if err != nil {
		return Page[Action]{Items: []Action{}}, err
	}
	if q.State != "" && !slices.Contains([]string{"pending", "executing", "unknown", "succeeded", "failed", "cancelled"}, q.State) {
		return Page[Action]{Items: []Action{}}, ErrInvalid
	}
	if q.Kind != "" && !slices.Contains(validActionKinds, q.Kind) {
		return Page[Action]{Items: []Action{}}, ErrInvalid
	}
	return listRecords[Action](s, ctx, p, g, "actions", ListQuery{Cursor: q.Cursor, Limit: q.Limit, Desc: desc, State: q.State, Kind: q.Kind})
}

func (s *Service) Actions(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Action], error) {
	return s.QueryActions(ctx, p, g, ActionQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) QueryReports(ctx context.Context, p account.Principal, g string, q ReportQuery) (Page[Report], error) {
	desc, err := parseOrder(q.Order)
	if err != nil {
		return Page[Report]{Items: []Report{}}, err
	}
	if q.State != "" && !slices.Contains([]string{"open", "suggested", "pending_execution", "resolved"}, q.State) {
		return Page[Report]{Items: []Report{}}, ErrInvalid
	}
	return listRecords[Report](s, ctx, p, g, "reports", ListQuery{Cursor: q.Cursor, Limit: q.Limit, Desc: desc, State: q.State})
}

func (s *Service) Reports(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Report], error) {
	return s.QueryReports(ctx, p, g, ReportQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) QueryHits(ctx context.Context, p account.Principal, g string, q HitQuery) (Page[Hit], error) {
	desc, err := parseOrder(q.Order)
	if err != nil {
		return Page[Hit]{Items: []Hit{}}, err
	}
	return listRecords[Hit](s, ctx, p, g, "hits", ListQuery{Cursor: q.Cursor, Limit: q.Limit, Desc: desc})
}

func (s *Service) Hits(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Hit], error) {
	return s.QueryHits(ctx, p, g, HitQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) QueryMessages(ctx context.Context, p account.Principal, g string, q MessageQuery) (Page[Message], error) {
	desc, err := parseOrder(q.Order)
	if err != nil {
		return Page[Message]{Items: []Message{}}, err
	}
	return listRecords[Message](s, ctx, p, g, "messages", ListQuery{Cursor: q.Cursor, Limit: q.Limit, Desc: desc, Recalled: q.Recalled})
}

func (s *Service) Messages(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Message], error) {
	return s.QueryMessages(ctx, p, g, MessageQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) Subscriptions(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Subscription], error) {
	return listRecords[Subscription](s, ctx, p, g, "subscriptions", ListQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) Blacklist(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Blacklist], error) {
	return listRecords[Blacklist](s, ctx, p, g, "blacklist", ListQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) Warnings(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Warnings], error) {
	return listRecords[Warnings](s, ctx, p, g, "warnings", ListQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) Watch(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Watch], error) {
	return listRecords[Watch](s, ctx, p, g, "watch", ListQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) Deliveries(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[Delivery], error) {
	return listRecords[Delivery](s, ctx, p, g, "deliveries", ListQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) Audits(ctx context.Context, p account.Principal, g, cursor string, limit int) (Page[AuditEntry], error) {
	return listRecords[AuditEntry](s, ctx, p, g, "audit", ListQuery{Cursor: cursor, Limit: limit})
}

func (s *Service) Groups(ctx context.Context, p account.Principal, cursor string, limit int) (Page[GroupSummary], error) {
	out := Page[GroupSummary]{Items: []GroupSummary{}}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if limit < 1 || limit > 100 || !validCursor(cursor, 4096) {
		return out, ErrInvalid
	}
	e := s.runRead(ctx, func(tx ReadTx) error {
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
