package groups_test

import (
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"testing"
)

func TestReportChecksCurrentMembership(t *testing.T) {
	f := setup(t)
	report := g.Command{Kind: "report", Message: "m", Text: "reason"}
	denied := func(group string) {
		t.Helper()
		if _, err := f.s.Command(ctx, user, group, "report", report); !errors.Is(err, g.ErrForbidden) {
			t.Fatalf("group %q: %v", group, err)
		}
	}
	denied("g")
	denied("missing")
	if count(t, f, "g/reports") != 0 {
		t.Fatal("unjoined report persisted")
	}
	join := event("join", "qq-bot", "g", "groups.member_joined", 1)
	if err := f.s.MemberEvent(ctx, "g", join, user.Key()); err != nil {
		t.Fatal(err)
	}
	command(t, f, user, "report", report)
	if err := f.s.MemberEvent(ctx, "g", event("leave", "qq-bot", "g", "groups.member_left", 2), user.Key()); err != nil {
		t.Fatal(err)
	}
	denied("g")
	if count(t, f, "g/reports") != 1 {
		t.Fatal("unexpected reports")
	}
}
