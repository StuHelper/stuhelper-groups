// Package adapters connects only public module APIs.
package adapters

import (
	"context"
	g "github.com/StuHelper/stuhelper-groups"
	q "github.com/StuHelper/stuhelper-qq-binding"
	v "github.com/StuHelper/stuhelper-verification"
	"slices"
)

type Binding interface {
	Status(context.Context, string) (q.Status, error)
}
type Verification interface {
	Facts(context.Context, v.Principal, string, string) (v.Summary, error)
}

type Facts struct {
	Binding      Binding
	Verification Verification
	Details      Details
	Principal    v.Principal
}

func (f Facts) Current(ctx context.Context, owner, school string) (g.Facts, error) {
	if f.Binding == nil {
		return g.Facts{}, g.ErrUnavailable
	}
	b, e := f.Binding.Status(ctx, owner)
	if e != nil {
		return g.Facts{}, e
	}
	out := g.Facts{Bound: b.Complete && b.OneBot.Bound && b.Connect.Bound && b.Official.Bound}
	if f.Verification != nil {
		s, e := f.Verification.Facts(ctx, f.Principal, owner, school)
		if e != nil {
			return g.Facts{}, e
		}
		out.Student = s.CampusEligibility
		out.StudentKnown = true
	}
	if f.Details != nil {
		details, e := f.Details.GroupFacts(ctx, f.Principal, owner, school)
		if e != nil {
			return g.Facts{}, e
		}
		if !details.StageKnown && (details.Alumni || details.Temporary) {
			return g.Facts{}, g.ErrUnavailable
		}
		out.Alumni = details.Alumni
		out.Temporary = details.Temporary
		out.StageKnown = details.StageKnown
		out.Fields = map[string]string{}
		for name, field := range details.Fields {
			if !slices.Contains([]string{"school", "grade", "department", "name", "student_id", "gender", "nickname"}, name) {
				return g.Facts{}, g.ErrUnavailable
			}
			if !field.Allowed || !field.Available {
				continue
			}
			if len(field.Value) > 2048 {
				return g.Facts{}, g.ErrUnavailable
			}
			if name == "gender" && !slices.Contains([]string{"male", "female", "unknown"}, field.Value) {
				return g.Facts{}, g.ErrUnavailable
			}
			out.Fields[name] = field.Value
		}
	}
	return out, nil
}
