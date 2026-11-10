package groups

import (
	"context"
	"regexp"
	"slices"
	"time"

	"github.com/StuHelper/stuhelper-platform/account"
)

// OwnerGroupRecord is internal repository data. Public owner queries still return
// only ManagedGroup; MyGroups explicitly projects a separate user-safe shape.
type OwnerGroupRecord struct {
	Config GroupConfig
	Member Member
}

type MyGroup struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	School            string     `json:"school"`
	Number            string     `json:"number"`
	Joined            bool       `json:"joined"`
	State             string     `json:"state"`
	Deadline          *time.Time `json:"deadline"`
	RequiresPassword  bool       `json:"requires_password"`
	Eligibility       string     `json:"eligibility"`
	MissingConditions []string   `json:"missing_conditions"`
	UnknownConditions []string   `json:"unknown_conditions"`
	NextActions       []string   `json:"next_actions"`
}

var maskedGroupNumber = regexp.MustCompile(`^\*+[0-9]{4}$`)

func publicGroupNumber(number string) (string, error) {
	if number == "" || maskedGroupNumber.MatchString(number) {
		return number, nil
	}
	if groupNumber.MatchString(number) {
		return maskNumber(number), nil
	}
	return "", ErrUnavailable
}

// MyGroups shares the indexed reverse lookup behind GroupsForOwner, including
// active applications. The only owner is the authenticated principal; no caller
// supplied owner, admin scope, or content fields enter the response.
func (s *Service) MyGroups(ctx context.Context, p account.Principal, cursor string, limit int) (Page[MyGroup], error) {
	out := Page[MyGroup]{Items: []MyGroup{}}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if limit < 1 || limit > 100 || len(cursor) > 128 {
		return out, ErrInvalid
	}
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	e := s.run(call, func(tx Tx) error {
		page, e := tx.OwnerGroupRecords(p.Key(), cursor, limit, true)
		if e != nil {
			return e
		}
		// Facts depend on owner/school, not on group. Cache within this request only.
		cache := map[factKey]Facts{}
		for _, record := range page.Items {
			c, m := record.Config, record.Member
			if m.Owner != p.Key() || !slices.Contains([]string{"requested", "awaiting_verification", "grace", "extended", "exempt", "restored", "removal_pending"}, m.State) {
				return ErrUnavailable
			}
			assessment, e := s.assessEligibility(call, tx, c, m, cache)
			if e != nil {
				return ErrUnavailable
			}
			number, e := publicGroupNumber(c.Number)
			if e != nil {
				return e
			}
			item := MyGroup{ID: c.ID, Name: c.Name, School: c.School, Number: number, Joined: m.Joined, State: m.State, RequiresPassword: c.Mode == "password", Eligibility: assessment.state, MissingConditions: assessment.missing, UnknownConditions: assessment.unknown, NextActions: assessment.actions}
			if !m.Deadline.IsZero() {
				deadline := m.Deadline.UTC()
				item.Deadline = &deadline
			}
			if item.Eligibility == "eligible" && (!m.Joined || (m.State != "restored" && m.State != "exempt")) {
				item.NextActions = append(item.NextActions, "refresh")
			}
			out.Items = append(out.Items, item)
		}
		out.NextCursor = page.NextCursor
		return nil
	})
	if e != nil {
		return Page[MyGroup]{Items: []MyGroup{}}, ErrUnavailable
	}
	return out, nil
}
