package groups_test

import (
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"testing"
	"time"
)

func TestExemptionAndMuteOutcome(t *testing.T) {
	f := setup(t)
	f.facts.set(false)
	if err := f.s.MemberEvent(ctx, "g", event("join", "qq-bot", "g", "groups.member_joined", 1), user.Key()); err != nil {
		t.Fatal(err)
	}
	command(t, f, manager, "extend", g.Command{Kind: "extend", Target: user.Key(), Text: "approved", Seconds: 100})
	command(t, f, manager, "exempt", g.Command{Kind: "exempt", Target: user.Key(), Text: "approved"})
	m, err := f.s.Progress(ctx, user, "g")
	if err != nil || m.Extended || !m.Deadline.IsZero() {
		t.Fatal(m, err)
	}
	command(t, f, user, "dice", g.Command{Kind: "dice"})
	for i, outcome := range []string{"failed", "succeeded", "cancelled"} {
		f.now = f.now.Add(31 * time.Second)
		r := command(t, f, user, "play-"+outcome, g.Command{Kind: "self_mute"})
		f.bot.failure = i == 0
		if i == 2 {
			command(t, f, manager, "ban", g.Command{Kind: "blacklist", Target: user.Key(), Text: "restricted"})
		}
		a, err := f.s.Execute(ctx, "g", r.ID)
		if err != nil || a.State != outcome {
			t.Fatal(a, err)
		}
		if i == 1 {
			u := command(t, f, manager, "unmute", g.Command{Kind: "unmute", Target: user.Key()})
			if _, err = f.s.Execute(ctx, "g", u.ID); err != nil {
				t.Fatal(err)
			}
		}
		m, err = f.s.Progress(ctx, user, "g")
		if err != nil || !m.MutedUntil.IsZero() {
			t.Fatal(m, err)
		}
	}
	if _, err = f.s.Command(ctx, user, "g", "blocked", g.Command{Kind: "dice"}); !errors.Is(err, g.ErrForbidden) {
		t.Fatal(err)
	}
}
