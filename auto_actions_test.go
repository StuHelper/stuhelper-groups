package groups_test

import (
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"testing"
	"time"
)

func TestAutomaticActionDurationAndReportLifecycle(t *testing.T) {
	f := setup(t)
	c := config()
	c.AutoActions = []string{"blacklist", "warn"}
	if _, err := configureCurrent(t, f, manager, "rules", c); err != nil {
		t.Fatal(err)
	}
	command(t, f, manager, "rule", g.Command{Kind: "rule", Text: "spam", Target: user.Key()})
	blocks, err := f.s.Blacklist(ctx, manager, "g", "", 10)
	if err != nil || len(blocks.Items) != 1 || !blocks.Items[0].Permanent {
		t.Fatal(blocks, err)
	}
	if err = f.r.Transaction(ctx, func(tx g.Tx) error {
		if err := tx.Put("g/messages", "m", g.Message{ID: "m", Owner: outsider.Key(), Expires: f.now.Add(time.Hour)}); err != nil {
			return err
		}
		return tx.Put("g/reports", "r", map[string]any{"message": "m", "state": "suggested", "suggestions": []string{"warn", "warn", "blacklist"}})
	}); err != nil {
		t.Fatal(err)
	}
	c.RetentionSeconds = 0
	if _, err = configureCurrent(t, f, manager, "off", c); err != nil {
		t.Fatal(err)
	}
	exec := g.Command{Kind: "ai_execute", Target: "r"}
	if _, err = f.s.Command(ctx, manager, "g", "closed", exec); !errors.Is(err, g.ErrNotFound) {
		t.Fatal(err)
	}
	c.RetentionSeconds = 120
	if _, err = configureCurrent(t, f, manager, "on", c); err != nil {
		t.Fatal(err)
	}
	command(t, f, manager, "exec", exec)
	if _, err = f.s.Command(ctx, manager, "g", "again", exec); !errors.Is(err, g.ErrConflict) {
		t.Fatal(err)
	}
	blocks, err = f.s.Blacklist(ctx, manager, "g", "", 10)
	if err != nil || len(blocks.Items) != 2 || !blocks.Items[0].Permanent || !blocks.Items[1].Permanent || count(t, f, "g/actions") != 2 {
		t.Fatal(blocks, err)
	}
}
