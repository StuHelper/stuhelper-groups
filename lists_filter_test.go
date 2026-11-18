package groups_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-groups/sdk"
	"github.com/StuHelper/stuhelper-platform/account"
)

func TestCollectionFiltersDescendingCursorsAndReportContext(t *testing.T) {
	f := setup(t)
	f.s.AI = ai{}
	author := account.Principal{Issuer: "test", Subject: "msg-author"}

	// Seed members with diverse states, joined and exempt flags.
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		specs := []struct {
			id     string
			state  string
			joined bool
			exempt bool
		}{
			{"m-01", "restored", true, false},
			{"m-02", "grace", true, false},
			{"m-03", "grace", false, false},
			{"m-04", "exempt", true, true},
			{"m-05", "removal_pending", true, false},
		}
		for _, s := range specs {
			if e := tx.Put("g/members", s.id, g.Member{Owner: s.id, State: s.state, Joined: s.joined, Exempt: s.exempt}); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}

	// Filter members by state + joined + exempt.
	w := api(t, f, manager, "GET", "/api/v1/groups/g/members?state=grace&joined=true", "", nil)
	if w.Code != 200 {
		t.Fatalf("members filter failed: %d %s", w.Code, w.Body.String())
	}
	shape(t, "MemberPage", w.Body.Bytes())
	var memberPage g.Page[g.Member]
	_ = json.Unmarshal(w.Body.Bytes(), &memberPage)
	if len(memberPage.Items) != 1 || memberPage.Items[0].ID != "m-02" {
		t.Fatalf("unexpected filtered members: %+v", memberPage)
	}

	exemptResp := api(t, f, manager, "GET", "/api/v1/groups/g/members?exempt=true", "", nil)
	_ = json.Unmarshal(exemptResp.Body.Bytes(), &memberPage)
	if len(memberPage.Items) != 1 || memberPage.Items[0].ID != "m-04" {
		t.Fatalf("unexpected exempt members: %+v", memberPage)
	}

	// Ingest messages (including recalled) at distinct and identical timestamps.
	t0 := f.now
	if e := f.s.Ingest(ctx, "g", event("ing-1", "qq-bot", "g", "groups.message", 1), g.Message{ID: "msg-1", Owner: author.Key(), Body: "sensitive message body"}); e != nil {
		t.Fatal(e)
	}
	f.now = t0.Add(10 * time.Second)
	if e := f.s.Ingest(ctx, "g", event("ing-2", "qq-bot", "g", "groups.message", 2), g.Message{ID: "msg-2", Owner: user.Key(), Body: "normal message"}); e != nil {
		t.Fatal(e)
	}
	f.now = t0.Add(20 * time.Second)
	if e := f.s.Ingest(ctx, "g", event("ing-3", "qq-bot", "g", "groups.message", 3), g.Message{ID: "msg-1", Owner: author.Key(), Recalled: true}); e != nil {
		t.Fatal(e)
	}

	// Messages filter recalled=true and preserve original created timestamp on recall.
	msgW := api(t, f, manager, "GET", "/api/v1/groups/g/messages?recalled=true&order=desc", "", nil)
	if msgW.Code != 200 {
		t.Fatalf("messages filter failed: %d %s", msgW.Code, msgW.Body.String())
	}
	shape(t, "MessagePage", msgW.Body.Bytes())
	var msgPage g.Page[g.Message]
	_ = json.Unmarshal(msgW.Body.Bytes(), &msgPage)
	if len(msgPage.Items) != 1 || msgPage.Items[0].ID != "msg-1" || msgPage.Items[0].Created == nil || !msgPage.Items[0].Created.Equal(t0) {
		t.Fatalf("unexpected recalled message page: %+v", msgPage)
	}

	// Report creation records target author & timestamps without copying message body; feedback records resolved_by.
	f.now = t0.Add(30 * time.Second)
	if err := f.s.MemberEvent(ctx, "g", event("reporter-joined", "qq-bot", "g", "groups.member_joined", 1), user.Key()); err != nil {
		t.Fatal(err)
	}
	rep1 := command(t, f, user, "rep-1", g.Command{Kind: "report", Message: "msg-1", Text: "harassment"})
	f.now = t0.Add(40 * time.Second)
	rep2 := command(t, f, user, "rep-2", g.Command{Kind: "report", Message: "missing-msg", Text: "other"})
	f.now = t0.Add(50 * time.Second)
	command(t, f, manager, "fb-1", g.Command{Kind: "feedback", Target: rep1.ID, Text: "handled"})

	openReports := api(t, f, manager, "GET", "/api/v1/groups/g/reports?state=open&order=desc", "", nil)
	if openReports.Code != 200 {
		t.Fatal(openReports.Code, openReports.Body.String())
	}
	shape(t, "ReportPage", openReports.Body.Bytes())
	var repPage g.Page[g.Report]
	_ = json.Unmarshal(openReports.Body.Bytes(), &repPage)
	if len(repPage.Items) != 1 || repPage.Items[0].ID != rep2.ID || repPage.Items[0].Target != "" || repPage.Items[0].Created == nil {
		t.Fatalf("unexpected open reports: %+v", repPage)
	}

	resolvedReports := api(t, f, manager, "GET", "/api/v1/groups/g/reports?state=resolved", "", nil)
	shape(t, "ReportPage", resolvedReports.Body.Bytes())
	if strings.Contains(resolvedReports.Body.String(), "sensitive message body") {
		t.Fatal("report leaked retained message body")
	}
	_ = json.Unmarshal(resolvedReports.Body.Bytes(), &repPage)
	if len(repPage.Items) != 1 || repPage.Items[0].Target != author.Key() || repPage.Items[0].ResolvedBy == nil || repPage.Items[0].ResolvedBy.Subject != manager.Subject || !repPage.Items[0].Created.Equal(t0.Add(30*time.Second)) || !repPage.Items[0].Updated.Equal(t0.Add(50*time.Second)) {
		t.Fatalf("unexpected resolved report context: %+v", repPage.Items)
	}

	// Hits, Blacklist and Watch persist created/updated timestamps (preserving created on update).
	f.now = t0.Add(60 * time.Second)
	command(t, f, manager, "hit-1", g.Command{Kind: "rule", Target: user.Key(), Message: "msg-2", Text: "spam"})
	command(t, f, manager, "blk-1", g.Command{Kind: "blacklist", Target: outsider.Key(), Text: "first"})
	command(t, f, manager, "watch-1", g.Command{Kind: "watch", Target: outsider.Key(), Enabled: true, Text: "watch-first"})
	f.now = t0.Add(70 * time.Second)
	command(t, f, manager, "blk-2", g.Command{Kind: "blacklist", Target: outsider.Key(), Text: "updated"})
	command(t, f, manager, "watch-2", g.Command{Kind: "watch", Target: outsider.Key(), Enabled: false, Text: "watch-off"})

	hitsPage, e := f.s.QueryHits(ctx, manager, "g", g.HitQuery{Limit: 10, Order: "desc"})
	if e != nil || len(hitsPage.Items) != 1 || hitsPage.Items[0].Created == nil || !hitsPage.Items[0].Created.Equal(t0.Add(60*time.Second)) {
		t.Fatalf("unexpected hits timestamps: %+v %v", hitsPage, e)
	}
	blkPage, e := f.s.Blacklist(ctx, manager, "g", "", 10)
	if e != nil || len(blkPage.Items) != 1 || blkPage.Items[0].Created == nil || !blkPage.Items[0].Created.Equal(t0.Add(60*time.Second)) || blkPage.Items[0].Updated == nil || !blkPage.Items[0].Updated.Equal(t0.Add(70*time.Second)) {
		t.Fatalf("unexpected blacklist timestamps: %+v %v", blkPage, e)
	}
	watchPage, e := f.s.Watch(ctx, manager, "g", "", 10)
	if e != nil || len(watchPage.Items) != 1 || watchPage.Items[0].Created == nil || !watchPage.Items[0].Created.Equal(t0.Add(60*time.Second)) || watchPage.Items[0].Updated == nil || !watchPage.Items[0].Updated.Equal(t0.Add(70*time.Second)) {
		t.Fatalf("unexpected watch timestamps: %+v %v", watchPage, e)
	}

	// Actions filter by state + kind and paginate in descending (created_at, id) order even with identical timestamps.
	f.now = t0.Add(80 * time.Second)
	for i := 0; i < 4; i++ {
		if i == 2 {
			f.now = t0.Add(90 * time.Second) // Two actions at +80s, two actions at +90s.
		}
		command(t, f, manager, fmt.Sprintf("act-warn-%d", i), g.Command{Kind: "warn", Target: user.Key(), Text: "warn"})
	}
	command(t, f, manager, "act-kick", g.Command{Kind: "kick", Target: user.Key()})

	seenIDs := []string{}
	cursor := ""
	for {
		u := "/api/v1/groups/g/actions?state=pending&kind=warn&order=desc&limit=2"
		if cursor != "" {
			u += "&cursor=" + cursor
		}
		actW := api(t, f, manager, "GET", u, "", nil)
		if actW.Code != 200 {
			t.Fatalf("actions desc page failed: %d %s", actW.Code, actW.Body.String())
		}
		shape(t, "ActionPage", actW.Body.Bytes())
		var page g.Page[g.Action]
		_ = json.Unmarshal(actW.Body.Bytes(), &page)
		for _, item := range page.Items {
			if item.State != "pending" || item.Command.Kind != "warn" {
				t.Fatalf("action filter leaked item: %+v", item)
			}
			seenIDs = append(seenIDs, item.ID)
		}
		if page.NextCursor == "" {
			break
		}
		// Passing a desc cursor to order=asc is rejected.
		problem(t, api(t, f, manager, "GET", "/api/v1/groups/g/actions?order=asc&cursor="+page.NextCursor, "", nil), 400, "groups_invalid_request")
		cursor = page.NextCursor
	}
	if len(seenIDs) != 4 {
		t.Fatalf("expected 4 warn actions across desc pages, got %d (%v)", len(seenIDs), seenIDs)
	}

	// Unknown and malformed query parameters are rejected across collection routes.
	for _, bad := range []string{
		"/api/v1/groups/g/members?unknown=1",
		"/api/v1/groups/g/members?state=invalid",
		"/api/v1/groups/g/members?joined=maybe",
		"/api/v1/groups/g/actions?order=sideways",
		"/api/v1/groups/g/actions?order=desc&cursor=plain-asc-id",
		"/api/v1/groups/g/actions?kind=invented",
		"/api/v1/groups/g/reports?state=invented",
		"/api/v1/groups/g/messages?recalled=1",
		"/api/v1/groups?school=buaa",
	} {
		problem(t, api(t, f, manager, "GET", bad, "", nil), 400, "groups_invalid_request")
	}

	// Generated Go SDK client supports the new filter parameters.
	server := httptest.NewServer(httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return manager, nil }))
	defer server.Close()
	client, e := sdk.NewClientWithResponses(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	state := sdk.ListActionsParamsState("pending")
	kind := sdk.ListActionsParamsKind("kick")
	order := sdk.ListActionsParamsOrder("desc")
	sdkRes, e := client.ListActionsWithResponse(ctx, "g", &sdk.ListActionsParams{State: &state, Kind: &kind, Order: &order})
	if e != nil || sdkRes.StatusCode() != http.StatusOK || sdkRes.JSON200 == nil || len(sdkRes.JSON200.Items) != 1 {
		t.Fatalf("sdk filtered actions failed: %+v %v", sdkRes, e)
	}
}

func TestMigrateLegacyCollectionAndPagePrefixedMessageIDs(t *testing.T) {
	f := setup(t)
	older := f.now
	for i, id := range []string{"d1:001", "d1:002"} {
		f.now = older.Add(time.Duration(i) * time.Second)
		if err := f.s.Ingest(ctx, "g", event("event-"+id, "qq-bot", "g", "groups.message", 1), g.Message{ID: id, Owner: user.Key(), Body: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx, "ALTER TABLE groups.records DROP COLUMN created_at"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE groups.records SET data=data-'created', query_changed_at=$1 WHERE bucket='g/messages' AND id='d1:002'", f.now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.r.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{"d1:001", "d1:002"} {
		var at time.Time
		if err := f.pool.QueryRow(ctx, "SELECT created_at FROM groups.records WHERE bucket='g/messages' AND id=$1", id).Scan(&at); err != nil || !at.Equal(older.Add(time.Duration(i)*time.Second)) {
			t.Fatal(id, at, err)
		}
	}
	first, err := f.s.QueryMessages(ctx, manager, "g", g.MessageQuery{Limit: 1})
	if err != nil || len(first.Items) != 1 || first.Items[0].ID != "d1:001" || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	second, err := f.s.QueryMessages(ctx, manager, "g", g.MessageQuery{Limit: 1, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != "d1:002" {
		t.Fatal(second, err)
	}
}
