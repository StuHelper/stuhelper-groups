package groups_test

import (
	g "github.com/StuHelper/stuhelper-groups"
	"testing"
	"time"
)

func TestJoinedAdmissionTransitions(t *testing.T) {
	f := setup(t)
	c := config()
	c.Mode = "muted"
	c.Welcome = "welcome"
	if _, err := configureCurrent(t, f, manager, "muted", c); err != nil {
		t.Fatal(err)
	}
	f.facts.set(false)
	command(t, f, user, "apply", g.Command{Kind: "apply"})
	m, err := f.s.Progress(ctx, user, "g")
	if err != nil || !m.Deadline.IsZero() || len(actions(t, f)) != 0 {
		t.Fatalf("actions before join: %+v %v", m, err)
	}
	if err = f.s.MemberEvent(ctx, "g", event("join", "qq-bot", "g", "groups.member_joined", 1), user.Key()); err != nil {
		t.Fatal(err)
	}
	n := func(kind string) int {
		i := 0
		for _, a := range actions(t, f) {
			if a.Command.Kind == kind {
				i++
			}
		}
		return i
	}
	if n("mute") != 1 || n("remind") != 1 {
		t.Fatal(actions(t, f))
	}
	f.facts.set(true)
	command(t, f, manager, "qualified", g.Command{Kind: "recheck", Target: user.Key()})
	c.Mode = "password"
	c.Password = strptr("fixture")
	if _, err = configureCurrent(t, f, manager, "password", c); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sweep(ctx, "g", ""); err != nil {
		t.Fatal(err)
	}
	m, err = f.s.Progress(ctx, user, "g")
	if err != nil || m.State != "restored" || n("welcome") != 1 {
		t.Fatal(m, err)
	}
	f.facts.set(false)
	command(t, f, manager, "lost", g.Command{Kind: "recheck", Target: user.Key()})
	f.now = f.now.Add(2 * time.Hour)
	r := command(t, f, manager, "expired", g.Command{Kind: "recheck", Target: user.Key()})
	f.bot.failure = true
	if a, e := f.s.Execute(ctx, "g", r.ID); e != nil || a.State != "failed" {
		t.Fatal(a, e)
	}
	if _, err = f.s.Sweep(ctx, "g", ""); err != nil || n("kick") != 2 {
		t.Fatal(actions(t, f), err)
	}
}

func TestPasswordJoinWelcomeAndManualRemoval(t *testing.T) {
	f := setup(t)
	c := config()
	c.Mode = "password"
	c.Password = strptr("sample")
	c.Welcome = "welcome"
	if _, err := configureCurrent(t, f, manager, "pass-mode", c); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MemberEvent(ctx, "g", event("join", "qq-bot", "g", "groups.member_joined", 1), user.Key()); err != nil {
		t.Fatal(err)
	}
	command(t, f, user, "password", g.Command{Kind: "password", Text: "sample"})
	if _, err := f.s.Sweep(ctx, "g", ""); err != nil {
		t.Fatal(err)
	}
	welcomes := 0
	for _, a := range actions(t, f) {
		if a.Command.Kind == "welcome" {
			welcomes++
		}
	}
	if welcomes != 1 {
		t.Fatal(welcomes)
	}
	r := command(t, f, manager, "manual", g.Command{Kind: "kick", Target: user.Key()})
	if _, err := f.s.Execute(ctx, "g", r.ID); err != nil {
		t.Fatal(err)
	}
	m, err := f.s.Progress(ctx, user, "g")
	if err != nil || m.Joined || m.State != "removed" {
		t.Fatal(m, err)
	}
}
