package adapters_test

import (
	"context"
	"errors"
	"testing"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/adapters"
	q "github.com/StuHelper/stuhelper-qq-binding"
	v "github.com/StuHelper/stuhelper-verification"
)

type binding struct{}

func (binding) Status(context.Context, string) (q.Status, error) {
	return q.Status{Complete: true, OneBot: q.Part{Bound: true}, Connect: q.Part{Bound: true}, Official: q.Part{Bound: true}}, nil
}

type verification struct{}

func (verification) Facts(context.Context, v.Principal, string, string) (v.Summary, error) {
	return v.Summary{CampusEligibility: true}, nil
}

type detailsFunc func(context.Context, v.Principal, string, string) (adapters.GroupDetails, error)

func (f detailsFunc) GroupFacts(c context.Context, p v.Principal, owner, school string) (adapters.GroupDetails, error) {
	return f(c, p, owner, school)
}

func TestGroupFactsPropagatesIdentityAndFiltersFieldPermissions(t *testing.T) {
	principal := v.Principal{Issuer: "test", Subject: "groups-service"}
	f := adapters.Facts{Binding: binding{}, Verification: verification{}, Principal: principal}
	f.Details = detailsFunc(func(_ context.Context, p v.Principal, owner, school string) (adapters.GroupDetails, error) {
		if p != principal || owner != "owner" || school != "school" {
			t.Fatal("authorization context not preserved")
		}
		return adapters.GroupDetails{StageKnown: true, Alumni: true, Fields: map[string]adapters.CardField{
			"name":       {Allowed: true, Available: true, Value: "获准姓名"},
			"student_id": {Allowed: false, Available: true, Value: "must-not-leak"},
			"department": {Allowed: true, Available: false, Value: "stale"},
			"gender":     {Allowed: true, Available: true, Value: "unknown"},
			"nickname":   {Allowed: true, Available: true, Value: ""},
		}}, nil
	})
	out, e := f.Current(context.Background(), "owner", "school")
	if e != nil {
		t.Fatal(e)
	}
	if !out.Bound || !out.Student || !out.StudentKnown || !out.StageKnown || !out.Alumni || out.Temporary {
		t.Fatalf("facts lost: %+v", out)
	}
	if len(out.Fields) != 3 || out.Fields["name"] != "获准姓名" || out.Fields["gender"] != "unknown" {
		t.Fatalf("disclosure mismatch: %+v", out.Fields)
	}
	if _, ok := out.Fields["nickname"]; !ok {
		t.Fatal("known empty field lost")
	}
}
func TestUnknownOrUnavailableFactsNeverBecomeKnownStage(t *testing.T) {
	f := adapters.Facts{Binding: binding{}, Verification: verification{}}
	for _, provider := range []adapters.Details{nil, detailsFunc(func(context.Context, v.Principal, string, string) (adapters.GroupDetails, error) {
		return adapters.GroupDetails{}, nil
	})} {
		f.Details = provider
		out, e := f.Current(context.Background(), "owner", "school")
		if e != nil || out.StageKnown || out.Alumni || out.Temporary {
			t.Fatalf("unknown facts widened: %+v %v", out, e)
		}
	}
	for _, want := range []error{g.ErrForbidden, g.ErrUnavailable, g.ErrNotFound, context.Canceled, context.DeadlineExceeded} {
		f.Details = detailsFunc(func(context.Context, v.Principal, string, string) (adapters.GroupDetails, error) {
			return adapters.GroupDetails{StageKnown: true, Alumni: true}, want
		})
		out, e := f.Current(context.Background(), "owner", "school")
		if !errors.Is(e, want) || out.StageKnown || out.Bound || len(out.Fields) > 0 {
			t.Fatalf("failed response used: %+v %v", out, e)
		}
	}
	for _, bad := range []adapters.GroupDetails{
		{Alumni: true},
		{StageKnown: true, Fields: map[string]adapters.CardField{"email": {Allowed: true, Available: true, Value: "unexpected"}}},
		{StageKnown: true, Fields: map[string]adapters.CardField{"gender": {Allowed: true, Available: true, Value: "guessed"}}},
	} {
		f.Details = detailsFunc(func(context.Context, v.Principal, string, string) (adapters.GroupDetails, error) { return bad, nil })
		if _, e := f.Current(context.Background(), "owner", "school"); !errors.Is(e, g.ErrUnavailable) {
			t.Fatalf("malformed facts accepted: %+v %v", bad, e)
		}
	}
}
