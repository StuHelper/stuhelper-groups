package groups_test

import (
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"strings"
	"testing"
)

func TestCardTemplateLiteralFieldsAndLimits(t *testing.T) {
	f := setup(t)
	f.facts.value.Fields["nickname"] = g.CardField{Allowed: true, Available: true, Value: "{separator}{昵称}"}
	r := command(t, f, manager, "card", g.Command{Kind: "card", Target: user.Key()})
	if r.Detail != "测试·{separator}{昵称}" {
		t.Fatal(r)
	}
	c := config()
	c.CardTemplate = "{unknown}"
	if _, err := configureCurrent(t, f, manager, "bad-template", c); !errors.Is(err, g.ErrUnprocessable) {
		t.Fatal(err)
	}
	c.CardTemplate = "{nickname}{nickname}"
	if _, err := configureCurrent(t, f, manager, "long-template", c); err != nil {
		t.Fatal(err)
	}
	f.facts.value.Fields["nickname"] = g.CardField{Allowed: true, Available: true, Value: strings.Repeat("a", 1025)}
	if _, err := f.s.Command(ctx, manager, "g", "long-card", g.Command{Kind: "card", Target: user.Key()}); !errors.Is(err, g.ErrUnprocessable) {
		t.Fatal(err)
	}
	if count(t, f, "g/actions") != 0 {
		t.Fatal("unexpected card actions")
	}
}
