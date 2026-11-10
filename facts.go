package groups

import (
	"context"
	"slices"
)

// currentFacts validates the module-owned contract before any business decision.
// Source-specific calls and error translation belong to backend, not this module.
func (s *Service) currentFacts(ctx context.Context, owner, school string) (Facts, error) {
	if s.Facts == nil {
		return Facts{}, ErrUnavailable
	}
	facts, err := s.Facts.GroupFacts(ctx, s.FactsPrincipal, owner, school)
	if err != nil {
		return Facts{}, err
	}
	if (!facts.StageKnown && (facts.Alumni || facts.Temporary)) || (!facts.StudentKnown && facts.Student) {
		return Facts{}, ErrUnavailable
	}
	fields := make(map[string]CardField)
	for name, field := range facts.Fields {
		if !slices.Contains([]string{"school", "grade", "department", "name", "student_id", "gender", "nickname"}, name) {
			return Facts{}, ErrUnavailable
		}
		if !field.Allowed || !field.Available {
			continue
		}
		if len(field.Value) > 2048 {
			return Facts{}, ErrUnavailable
		}
		if name == "gender" && !slices.Contains([]string{"male", "female", "unknown"}, field.Value) {
			return Facts{}, ErrUnavailable
		}
		fields[name] = field
	}
	facts.Fields = fields
	return facts, nil
}
