package groups

import (
	"context"
	"errors"
	"slices"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/StuHelper/stuhelper-platform/account"
)

// DirectoryRecord joins a discoverable group configuration with the caller's own
// non-removed membership/application record when one exists.
type DirectoryRecord struct {
	Config GroupConfig
	Member *Member
}

type DirectoryGroup struct {
	ID               string `json:"id"`
	School           string `json:"school"`
	Name             string `json:"name"`
	Number           string `json:"number"`
	Category         string `json:"category"`
	Description      string `json:"description"`
	Mode             string `json:"mode"`
	RequireStudent   bool   `json:"require_student"`
	AcceptAlumni     bool   `json:"accept_alumni"`
	AcceptTemporary  bool   `json:"accept_temporary"`
	RequiresPassword bool   `json:"requires_password"`
	PasswordHint     string `json:"password_hint,omitempty"`
	Joined           bool   `json:"joined"`
	State            string `json:"state"`
}

type DirectoryQuery struct {
	School   string
	Category string
	Cursor   string
	Limit    int
}

func validCursor(v string, maxLen int) bool {
	return len(v) <= maxLen && utf8.ValidString(v) && !containsControlOrSlash(v)
}

func containsControlOrSlash(v string) bool {
	for _, r := range v {
		if unicode.IsControl(r) || r == '/' {
			return true
		}
	}
	return false
}

func activeBlacklist(tx Tx, group, owner string, now time.Time) (bool, error) {
	var block Blacklist
	e := tx.Get(bucket(group, "blacklist"), owner, &block)
	if errors.Is(e, ErrNotFound) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return block.Permanent || now.Before(block.Until), nil
}

// Directory returns student-facing public cards for discoverable groups. Groups
// requiring student verification are shown only when the caller holds valid campus
// eligibility in that school; groups without student requirement remain publicly
// discoverable to authenticated users unless restricted by blacklist.
func (s *Service) Directory(ctx context.Context, p account.Principal, q DirectoryQuery) (Page[DirectoryGroup], error) {
	out := Page[DirectoryGroup]{Items: []DirectoryGroup{}}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if q.Limit < 1 || q.Limit > 100 || !validCursor(q.Cursor, 128) {
		return out, ErrInvalid
	}
	if q.School != "" && (q.School == "*" || !valid(q.School, 128)) {
		return out, ErrInvalid
	}
	if q.Category != "" && !slices.Contains([]string{"school", "department", "grade", "course", "club"}, q.Category) {
		return out, ErrInvalid
	}
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	e := s.run(call, func(tx Tx) error {
		globallyBlocked, err := activeBlacklist(tx, "*", p.Key(), s.now())
		if err != nil {
			return err
		}
		if globallyBlocked {
			return ErrForbidden
		}
		rows, err := tx.DirectoryGroups(p.Key(), q.School, q.Category, q.Cursor, q.Limit)
		if err != nil {
			return err
		}
		cache := map[factKey]Facts{}
		for _, record := range rows {
			c := record.Config
			out.NextCursor = c.ID
			blocked, err := activeBlacklist(tx, c.ID, p.Key(), s.now())
			if err != nil {
				return err
			}
			if blocked {
				continue
			}
			if c.RequireStudent {
				key := factKey{p.Key(), c.School}
				facts, found := cache[key]
				if !found {
					facts, err = s.currentFacts(call, p.Key(), c.School)
					if err != nil {
						return ErrUnavailable
					}
					cache[key] = facts
				}
				if !facts.StudentKnown || !facts.Student {
					continue
				}
			}
			number, err := publicGroupNumber(c.Number)
			if err != nil {
				return err
			}
			joined := false
			state := ""
			if record.Member != nil {
				if record.Member.Owner != p.Key() || !slices.Contains([]string{"requested", "awaiting_verification", "grace", "extended", "exempt", "restored", "removal_pending"}, record.Member.State) {
					return ErrUnavailable
				}
				joined = record.Member.Joined
				state = record.Member.State
			}
			item := DirectoryGroup{
				ID:               c.ID,
				School:           c.School,
				Name:             c.Name,
				Number:           number,
				Category:         c.Category,
				Description:      c.Description,
				Mode:             c.Mode,
				RequireStudent:   c.RequireStudent,
				AcceptAlumni:     c.AcceptAlumni,
				AcceptTemporary:  c.AcceptTemporary,
				RequiresPassword: c.Mode == "password",
				Joined:           joined,
				State:            state,
			}
			if item.RequiresPassword && c.PasswordHint != "" {
				item.PasswordHint = c.PasswordHint
			}
			out.Items = append(out.Items, item)
		}
		if len(rows) < q.Limit {
			out.NextCursor = ""
		}
		return nil
	})
	if e != nil {
		if errors.Is(e, ErrForbidden) {
			return Page[DirectoryGroup]{Items: []DirectoryGroup{}}, ErrForbidden
		}
		if errors.Is(e, ErrInvalid) {
			return Page[DirectoryGroup]{Items: []DirectoryGroup{}}, ErrInvalid
		}
		return Page[DirectoryGroup]{Items: []DirectoryGroup{}}, ErrUnavailable
	}
	return out, nil
}
