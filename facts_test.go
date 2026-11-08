package groups_test

import (
	"context"
	"errors"
	"testing"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
)

type groupFactsFunc func(context.Context, account.Principal, string, string) (g.Facts, error)

func (f groupFactsFunc) GroupFacts(ctx context.Context, p account.Principal, owner, school string) (g.Facts, error) {
	return f(ctx, p, owner, school)
}

func TestGroupFactsFiltersFieldsAtServiceBoundary(t *testing.T) {
	f := setup(t)
	principal := account.Principal{Issuer: "test", Subject: "groups-service"}
	f.s.FactsPrincipal = principal
	fields := map[string]g.CardField{
		"name":       {Allowed: true, Available: true, Value: "获准姓名"},
		"student_id": {Allowed: false, Available: true, Value: "must-not-leak"},
		"department": {Allowed: true, Available: false, Value: "stale"},
		"gender":     {Allowed: true, Available: true, Value: "unknown"},
		"nickname":   {Allowed: true, Available: true, Value: ""},
	}
	f.s.Facts = groupFactsFunc(func(_ context.Context, p account.Principal, owner, school string) (g.Facts, error) {
		if p != principal || owner != user.Key() || school != "school" {
			t.Fatal("authorization context not preserved")
		}
		return g.Facts{Bound: true, StudentKnown: true, Student: true, StageKnown: true, Fields: fields}, nil
	})
	result := command(t, f, manager, "card", g.Command{Kind: "card", Target: user.Key()})
	if result.State != "noncompliant" || result.Detail != "获准姓名·" {
		t.Fatalf("known fields/empty nickname lost: %+v", result)
	}
	for _, name := range []string{"student_id", "department"} {
		c := config()
		c.CardTemplate = "{" + name + "}"
		c.AutoCard = true
		c.RemindCard = true
		if _, err := f.s.Configure(ctx, manager, "config-"+name, c); err != nil {
			t.Fatal(err)
		}
		result = command(t, f, manager, "card-"+name, g.Command{Kind: "card", Target: user.Key()})
		if result.State != "pending_profile" || result.Detail != "" || count(t, f, "g/actions") != 0 {
			t.Fatalf("denied/missing field used: %+v", result)
		}
	}
	if len(fields) != 5 || fields["student_id"].Value != "must-not-leak" {
		t.Fatal("provider-owned map mutated")
	}
}

func TestGroupFactsRejectsUnknownContradictoryAndFailedFacts(t *testing.T) {
	f := setup(t)
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		return g.Facts{Bound: true, StudentKnown: true, Student: true}, nil
	})
	result := command(t, f, user, "unknown", g.Command{Kind: "apply"})
	if result.State != "awaiting_verification" {
		t.Fatalf("unknown stage granted entry: %+v", result)
	}
	beforeMembers, beforeActions := count(t, f, "g/members"), count(t, f, "g/actions")
	for i, want := range []error{g.ErrForbidden, g.ErrUnavailable, g.ErrNotFound, context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
				return g.Facts{Bound: true, Student: true, StudentKnown: true, StageKnown: true, Fields: map[string]g.CardField{"name": {Allowed: true, Available: true, Value: "partial"}}}, want
			})
			got, err := f.s.Command(ctx, manager, "g", string(rune('a'+i)), g.Command{Kind: "card", Target: user.Key()})
			if !errors.Is(err, g.ErrUnavailable) || got.Detail != "" {
				t.Fatalf("partial facts used: %+v %v", got, err)
			}
			_, err = f.s.Command(ctx, outsider, "g", string(rune('k'+i)), g.Command{Kind: "apply"})
			if !errors.Is(err, g.ErrUnavailable) || count(t, f, "g/members") != beforeMembers || count(t, f, "g/actions") != beforeActions {
				t.Fatal("failed dependency mutated admission", err)
			}
		})
	}
	for i, bad := range []g.Facts{
		{Alumni: true}, {Temporary: true}, {Student: true},
		{Fields: map[string]g.CardField{"email": {Allowed: true, Available: true, Value: "unexpected"}}},
		{Fields: map[string]g.CardField{"gender": {Allowed: true, Available: true, Value: "guessed"}}},
	} {
		f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) { return bad, nil })
		got, err := f.s.Command(ctx, manager, "g", string(rune('u'+i)), g.Command{Kind: "card", Target: user.Key()})
		if !errors.Is(err, g.ErrUnavailable) || got.Detail != "" || count(t, f, "g/actions") != beforeActions {
			t.Fatalf("invalid facts accepted: %+v %v", got, err)
		}
	}
}
