package groups_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
)

func TestGroupRelationshipCursorCountsUnicodeCodePoints(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}
	firstID := "a" + strings.Repeat("🧪", 127)
	secondID := "b-next"
	for _, id := range []string{firstID, secondID} {
		in := config()
		in.ID = id
		in.Discoverable = true
		c, err := f.s.Configure(ctx, root, "cursor-"+id[:1], in)
		if err != nil {
			t.Fatal(err)
		}
		putOwned(t, f, c, user, g.Member{Joined: true, State: "restored"})
	}
	tooLong := firstID + "界"

	t.Run("my groups", func(t *testing.T) {
		first := personalPage(t, f, user, "?limit=1")
		if len(first.Items) != 1 || first.Items[0].ID != firstID || first.NextCursor != firstID {
			t.Fatal("unexpected first personal page", first)
		}
		next := personalPage(t, f, user, "?cursor="+url.QueryEscape(first.NextCursor))
		if len(next.Items) != 1 || next.Items[0].ID != secondID || next.NextCursor != "" {
			t.Fatal("unexpected next personal page", next)
		}
		w := api(t, f, user, "GET", "/api/v1/groups/me?cursor="+url.QueryEscape(tooLong), "", nil)
		problem(t, w, 400, "groups_invalid_request")
	})

	t.Run("directory", func(t *testing.T) {
		first := directoryPage(t, f, user, "?limit=1")
		if len(first.Items) != 1 || first.Items[0].ID != firstID || first.NextCursor != firstID {
			t.Fatal("unexpected first directory page", first)
		}
		next := directoryPage(t, f, user, "?cursor="+url.QueryEscape(first.NextCursor))
		if len(next.Items) != 1 || next.Items[0].ID != secondID || next.NextCursor != "" {
			t.Fatal("unexpected next directory page", next)
		}
		w := api(t, f, user, "GET", "/api/v1/groups/directory?cursor="+url.QueryEscape(tooLong), "", nil)
		problem(t, w, 400, "groups_invalid_request")
	})

	t.Run("owner lookup", func(t *testing.T) {
		first, err := f.s.GroupsForOwner(ctx, user, "", 1)
		if err != nil || len(first.Items) != 1 || first.Items[0].ID != firstID || first.NextCursor != firstID {
			t.Fatal("unexpected first owner page", first, err)
		}
		next, err := f.s.GroupsForOwner(ctx, user, first.NextCursor, 2)
		if err != nil || len(next.Items) != 1 || next.Items[0].ID != secondID || next.NextCursor != "" {
			t.Fatal("unexpected next owner page", next, err)
		}
		if _, err := f.s.GroupsForOwner(ctx, user, tooLong, 2); !errors.Is(err, g.ErrInvalid) {
			t.Fatalf("oversized owner cursor returned %v", err)
		}
	})
}
