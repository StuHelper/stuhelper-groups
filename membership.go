package groups

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/StuHelper/stuhelper-platform/account"
)

// ManagedGroup identifies a configured group; it deliberately omits card fields,
// messages, raw QQ identifiers and other membership data.
type ManagedGroup struct {
	ID     string `json:"id"`
	School string `json:"school"`
}

// GroupsForOwner is a trusted backend query, not an HTTP authorization bypass.
// It returns joined, non-removed memberships whose group configuration exists.
func (s *Service) GroupsForOwner(ctx context.Context, owner account.Principal, cursor string, limit int) (Page[ManagedGroup], error) {
	out := Page[ManagedGroup]{Items: []ManagedGroup{}}
	if !owner.Valid() || len(cursor) > 128 || limit < 1 || limit > 100 {
		return out, ErrInvalid
	}
	e := s.run(ctx, func(tx Tx) error { var e error; out, e = tx.OwnerGroups(owner.Key(), cursor, limit, false); return e })
	if e != nil {
		return Page[ManagedGroup]{Items: []ManagedGroup{}}, e
	}
	return out, nil
}

// UpstreamEvent uses the providers' public CloudEvents shape. Preserve source
// and subject: verification uses object streams; qq-binding uses an owner stream.
type UpstreamEvent struct {
	SpecVersion     string            `json:"specversion"`
	ID              string            `json:"id"`
	Source          string            `json:"source"`
	Type            string            `json:"type"`
	Subject         string            `json:"subject"`
	Time            time.Time         `json:"time"`
	DataContentType string            `json:"datacontenttype"`
	SchoolID        string            `json:"schoolid,omitempty"`
	Data            EligibilityChange `json:"data"`
}
type EligibilityChange struct {
	Owner    string `json:"owner"`
	Revision int64  `json:"revision"`
}

// A bounded transaction provides all-or-nothing fanout. Overflow is a failure,
// not a partial acknowledgement; Sweep remains available for periodic recovery.
const MaxEventGroups = 1000

var upstreamType = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)+\.v1$`)

func validUpstream(e UpstreamEvent) bool {
	if e.SpecVersion != "1.0" || e.DataContentType != "application/json" || e.Time.IsZero() || e.Data.Revision < 1 || !eventText(e.ID, 256) || !eventText(e.Subject, 2048) || !eventText(e.Type, 128) || (e.SchoolID != "" && !eventText(e.SchoolID, 128)) {
		return false
	}
	if _, err := account.Parse(e.Data.Owner); err != nil {
		return false
	}
	if !upstreamType.MatchString(e.Type) {
		return false
	}
	switch e.Source {
	case "verification":
		return upstreamFamily(e.Type, "verification.credential.") || upstreamFamily(e.Type, "verification.teacher.") || e.Type == "verification.identity.verified.v1"
	case "qq-binding":
		if e.Type == "qq-binding.owner-bindings.changed.v1" {
			// qq-binding 49df06d allocates one durable positive int64 ID, formats it
			// in base 10 for the envelope, and uses it as the owner stream revision.
			return e.Subject == e.Data.Owner && e.ID == strconv.FormatInt(e.Data.Revision, 10)
		}
		return upstreamFamily(e.Type, "qq-binding.binding.") || upstreamFamily(e.Type, "qq-binding.unlink.") || e.Type == "qq-binding.account.closed.v1"
	default:
		return false
	}
}

// A family requires a nonempty subtype before the version suffix.
func upstreamFamily(kind, prefix string) bool {
	return strings.HasPrefix(kind, prefix) && len(strings.TrimSuffix(kind, ".v1")) > len(prefix)
}
func eventText(v string, n int) bool {
	return strings.TrimSpace(v) != "" && len(v) <= n && utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
}

// Consume atomically records inbox/revision and reevaluates all affected groups.
// Source data only identifies the owner: current facts always come from Facts.
func (s *Service) Consume(ctx context.Context, event UpstreamEvent) error {
	if !validUpstream(event) {
		return ErrInvalid
	}
	payload, e := json.Marshal(event)
	if e != nil {
		return ErrInvalid
	}
	envelope := Event{SpecVersion: event.SpecVersion, ID: event.ID, Source: event.Source, Type: event.Type, Subject: event.Subject, Time: event.Time, Revision: event.Data.Revision, Data: payload}
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.run(call, func(tx Tx) error {
		accepted, e := tx.Accept(envelope)
		if e != nil || !accepted {
			return e
		}
		// Include active applications as well as joined members. Fetch the complete
		// bounded set before touching state so overflow cannot leave partial changes.
		groups := []ManagedGroup{}
		cursor := ""
		for {
			page, e := tx.OwnerGroups(event.Data.Owner, cursor, 100, true)
			if e != nil {
				return e
			}
			groups = append(groups, page.Items...)
			if len(groups) > MaxEventGroups {
				return ErrUnavailable
			}
			if page.NextCursor == "" {
				break
			}
			if len(groups) == MaxEventGroups {
				return ErrUnavailable
			}
			cursor = page.NextCursor
		}
		for _, group := range groups {
			if e = call.Err(); e != nil {
				return e
			}
			var c GroupConfig
			if e = tx.Get("config", group.ID, &c); e != nil {
				return e
			}
			if _, e = s.admission(call, tx, account.Principal{}, c, event.Data.Owner, false); e != nil {
				return e
			}
		}
		return nil
	})
}
