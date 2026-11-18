package groups_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
	"testing"
)

func TestReceiptScopeAndLegacyReplay(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}
	key := "same/key"
	c := config()
	c.ID = "second"
	if _, err := f.s.Configure(ctx, root, key, c); err != nil {
		t.Fatal(err)
	}
	warn := g.Command{Kind: "warn", Target: user.Key(), Text: "reason"}
	for _, id := range []string{"g", "second"} {
		for range 2 {
			if _, err := f.s.Command(ctx, root, id, key, warn); err != nil {
				t.Fatal(err)
			}
		}
		if count(t, f, id+"/actions") != 1 {
			t.Fatal("duplicate action", id)
		}
	}
	changed := warn
	changed.Text = "different"
	if _, err := f.s.Command(ctx, root, "g", key, changed); !errors.Is(err, g.ErrConflict) {
		t.Fatal(err)
	}
	digest := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(b))
	}
	oldID := digest([]string{root.Key(), "old"})
	input := struct {
		Group   string
		Command g.Command
	}{"g", warn}
	err := f.r.Transaction(ctx, func(tx g.Tx) error {
		return tx.Put("receipts", oldID, map[string]any{"Digest": digest(input), "Result": g.Result{ID: "old-action", State: "pending"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.Command(ctx, root, "g", "old", warn)
	if err != nil || r.ID != "old-action" || count(t, f, "g/actions") != 1 {
		t.Fatal(r, err)
	}
}
