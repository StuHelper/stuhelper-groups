package groups

import (
	"context"
	"errors"
	"slices"
)

type factKey struct{ owner, school string }
type eligibilityAssessment struct {
	state                     string
	missing, unknown, actions []string
}

func assessment() eligibilityAssessment {
	return eligibilityAssessment{state: "eligible", missing: []string{}, unknown: []string{}, actions: []string{}}
}
func (a *eligibilityAssessment) action(value string) {
	if !slices.Contains(a.actions, value) {
		a.actions = append(a.actions, value)
	}
}

// assessEligibility is the single prerequisite decision used by admission,
// execution-time checks and the user's read-only progress view. No write occurs.
func (s *Service) assessEligibility(ctx context.Context, tx Tx, c GroupConfig, m Member, cache map[factKey]Facts) (eligibilityAssessment, error) {
	out := assessment()
	for _, group := range []string{c.ID, "*"} {
		var block Blacklist
		e := tx.Get(bucket(group, "blacklist"), m.Owner, &block)
		if e == nil && (block.Permanent || s.now().Before(block.Until)) {
			out.state = "ineligible"
			out.missing = append(out.missing, "restricted")
			out.action("contact_group_admin")
			return out, nil
		}
		if e != nil && !errors.Is(e, ErrNotFound) {
			return out, e
		}
	}
	key := factKey{m.Owner, c.School}
	facts, found := cache[key]
	if !found {
		var e error
		facts, e = s.currentFacts(ctx, m.Owner, c.School)
		if e != nil {
			return assessment(), ErrUnavailable
		}
		if cache != nil {
			cache[key] = facts
		}
	}
	if !facts.Bound {
		out.missing = append(out.missing, "qq_binding")
		out.action("bind_qq")
	}
	if c.RequireStudent {
		switch {
		case !facts.StudentKnown:
			out.unknown = append(out.unknown, "student_eligibility")
			out.action("refresh")
		case !facts.Student:
			out.missing = append(out.missing, "student_verification")
			out.action("verify_student")
		case !facts.StageKnown:
			out.unknown = append(out.unknown, "student_stage")
			out.action("refresh")
		default:
			if facts.Alumni && !c.AcceptAlumni {
				out.missing = append(out.missing, "alumni_not_accepted")
				out.action("contact_group_admin")
			}
			if facts.Temporary && !c.AcceptTemporary {
				out.missing = append(out.missing, "temporary_not_accepted")
				out.action("contact_group_admin")
			}
		}
	}
	if c.Mode == "password" && !m.Password {
		out.missing = append(out.missing, "password")
		if c.PasswordSet {
			out.action("submit_password")
		} else {
			out.action("contact_group_admin")
		}
	}
	if len(out.unknown) > 0 {
		out.state = "unknown"
	} else if len(out.missing) > 0 {
		out.state = "ineligible"
	}
	return out, nil
}
