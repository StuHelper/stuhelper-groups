package groups_test

import (
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"strings"
	"testing"
)

func TestTextValidationAtBoundaries(t *testing.T) {
	f := setup(t)
	for _, path := range []string{"/g%00/members", "/g%FF/progress", "/me?cursor=%00", "/g/messages?cursor=%FF"} {
		w := api(t, f, manager, "GET", "/api/v1/groups"+path, "", nil)
		if w.Code != 400 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if _, err := f.s.Command(ctx, manager, "g", "nul", g.Command{Kind: "send", Text: "bad\x00"}); !errors.Is(err, g.ErrInvalid) {
		t.Error(err)
	}
	c := config()
	c.Welcome = strings.Repeat("群", 2048)
	c.RulesTemplate = strings.Repeat("规", 8192)
	c.Keywords = []string{strings.Repeat("字", 256), strings.Repeat("词", 256), strings.Repeat("文", 256)}
	c.ExpectedRevision = 1
	w := api(t, f, manager, "PUT", "/api/v1/groups"+"/g", "unicode", c)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	command(t, f, manager, "valid", g.Command{Kind: "send", Text: strings.Repeat("字", 8192)})
	if count(t, f, "g/actions") != 1 {
		t.Fatal("unexpected action writes")
	}
}
