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
func (s *Service) assessEligibility(ctx context.Context, tx ReadTx, c GroupConfig, m Member, cache map[factKey]Facts) (eligibilityAssessment, error) {
	out := assessment()
	restricted, err := s.restricted(tx, c.ID, m.Owner)
	if err != nil {
		return out, err
	}
	if restricted {
		out.state = "ineligible"
		out.missing = append(out.missing, "restricted")
		out.action("contact_group_admin")
		return out, nil
	}
	key := factKey{m.Owner, c.School}
	facts, found := cache[key]
	if !found {
		var e error
		facts, e = s.transactionFacts(ctx, tx, key)
		if e != nil {
			return assessment(), e
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
	needsPassword := c.Mode == "password" && (!m.Password || m.PasswordEpoch < c.PasswordEpoch)
	if needsPassword && m.Joined {
		var history admissionHistory
		err := tx.Get(bucket(c.ID, "admissions"), m.Owner, &history)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return out, err
		}
		needsPassword = !history.Admitted && m.State != "restored"
	}
	if needsPassword {
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

func (s *Service) restricted(tx ReadTx, id, owner string) (bool, error) {
	for _, group := range []string{id, "*"} {
		var block Blacklist
		e := tx.Get(bucket(group, "blacklist"), owner, &block)
		if e == nil && (block.Permanent || s.now().Before(block.Until)) {
			return true, nil
		}
		if e != nil && !errors.Is(e, ErrNotFound) {
			return false, e
		}
	}
	return false, nil
}

func (s *Service) canPlay(ctx context.Context, tx Tx, c GroupConfig, m Member) (bool, error) {
	if !m.Joined || m.State == "removed" {
		return false, nil
	}
	if m.Exempt {
		blocked, err := s.restricted(tx, c.ID, m.Owner)
		return !blocked, err
	}
	return s.eligible(ctx, tx, c, m)
}
