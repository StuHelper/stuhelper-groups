package groups_test

import (
	"context"
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
	"strings"
	"testing"
)

func TestBatchPreflightAndRetry(t *testing.T) {
	f := setup(t)
	in := g.Bulk{Targets: []string{user.Key(), outsider.Key()}, Command: g.Command{Kind: "card"}, Preview: true}
	if _, err := f.s.Batch(ctx, user, "g", "preview", in); !errors.Is(err, g.ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := f.s.Batch(ctx, manager, "missing", "preview", in); !errors.Is(err, g.ErrNotFound) {
		t.Fatal(err)
	}
	if count(t, f, "batch_inputs") != 0 {
		t.Fatal("preview wrote a receipt")
	}
	in.Preview = false
	key := strings.Repeat("k", 128)
	calls := map[string]int{}
	f.s.Facts = groupFactsFunc(func(_ context.Context, _ account.Principal, owner, school string) (g.Facts, error) {
		calls[owner]++
		if owner == outsider.Key() && calls[owner] == 1 {
			return g.Facts{}, g.ErrUnavailable
		}
		return f.facts.value, nil
	})
	if _, err := f.s.Batch(ctx, manager, "g", key, in); !errors.Is(err, g.ErrUnavailable) {
		t.Fatal(err)
	}
	if count(t, f, "batch_inputs") != 1 {
		t.Fatal("missing batch digest")
	}
	page, err := f.s.Batch(ctx, manager, "g", key, in)
	if err != nil || len(page.Items) != 2 || calls[user.Key()] != 1 || calls[outsider.Key()] != 2 {
		t.Fatal(page, err, calls)
	}
}
