package groups_test

import (
	g "github.com/StuHelper/stuhelper-groups"
	"testing"
)

func TestRuleLabels(t *testing.T) {
	f := setup(t)
	c := config()
	c.BlockLinks = true
	c.Keywords = []string{"one", "two"}
	if _, err := configureCurrent(t, f, manager, "rules", c); err != nil {
		t.Fatal(err)
	}
	cmd := g.Command{Kind: "rule", Target: user.Key(), Text: "one two HtTpS://example.test"}
	r := command(t, f, manager, "match", cmd)
	if r.Detail != "link,keyword" {
		t.Fatal(r)
	}
	hits, err := f.s.Hits(ctx, manager, "g", "", 10)
	if err != nil || len(hits.Items) != 1 || len(hits.Items[0].Rules) != 2 {
		t.Fatal(hits, err)
	}
}
