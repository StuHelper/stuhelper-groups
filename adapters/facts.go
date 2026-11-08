// Package adapters connects only public module APIs.
package adapters

import (
	"context"
	g "github.com/StuHelper/stuhelper-groups"
	q "github.com/StuHelper/stuhelper-qq-binding"
	v "github.com/StuHelper/stuhelper-verification"
)

type Binding interface {
	Status(context.Context, string) (q.Status, error)
}
type Verification interface {
	Facts(context.Context, v.Principal, string, string) (v.Summary, error)
}

// Details supplies explicitly authorized stage/profile fields. Summary alone
// cannot distinguish alumni or temporary entrants, so nil fails closed.
type Details interface {
	GroupFacts(context.Context, string, string) (alumni, temporary bool, fields map[string]string, err error)
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
			return out, e
		}
		out.Student = s.CampusEligibility
		out.StudentKnown = true
	}
	if f.Details != nil {
		a, t, fields, e := f.Details.GroupFacts(ctx, owner, school)
		if e != nil {
			return out, e
		}
		out.Alumni = a
		out.Temporary = t
		out.Fields = fields
		out.StageKnown = true
	}
	return out, nil
}
