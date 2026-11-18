package groups_test

import (
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
	"testing"
)

func TestDisableExistingSubscriptionWithSourceGrant(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}
	c := config()
	c.ID = "receiver"
	if _, err := f.s.Configure(ctx, root, "receiver", c); err != nil {
		t.Fatal(err)
	}
	sub := g.Command{Kind: "subscribe", Receiver: "receiver", Enabled: true}
	command(t, f, root, "enable", sub)
	if _, err := f.s.Command(ctx, manager, "g", "denied", sub); !errors.Is(err, g.ErrForbidden) {
		t.Fatal(err)
	}
	before := count(t, f, "g/audit")
	sub.Enabled = false
	command(t, f, manager, "disable", sub)
	command(t, f, manager, "disable", sub)
	page, err := f.s.Subscriptions(ctx, manager, "g", "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Enabled || count(t, f, "g/audit") != before+1 {
		t.Fatal(page, err)
	}
	sub.Receiver = "absent"
	if _, err = f.s.Command(ctx, manager, "g", "absent", sub); !errors.Is(err, g.ErrNotFound) {
		t.Fatal(err)
	}
}
