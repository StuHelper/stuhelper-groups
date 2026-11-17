package groups_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-groups/sdk"
	"github.com/StuHelper/stuhelper-platform/account"
)

func directoryPage(t *testing.T, f *fixture, p account.Principal, query string) g.Page[g.DirectoryGroup] {
	t.Helper()
	w := api(t, f, p, "GET", "/api/v1/groups/directory"+query, "", nil)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	shape(t, "DirectoryGroupPage", w.Body.Bytes())
	var out g.Page[g.DirectoryGroup]
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	return out
}

func TestGroupDirectoryDiscoveryFilteringVisibilityAndPasswordHint(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}

	// Configure groups across categories, schools and visibility modes.
	courseGroup := config()
	courseGroup.ID = "buaa-course-os"
	courseGroup.School = "buaa"
	courseGroup.Name = "操作系统课程群"
	courseGroup.Discoverable = true
	courseGroup.Category = "course"
	courseGroup.Description = "课程答疑与通知交流"
	courseGroup.Number = strptr("987654321")
	if _, e := f.s.Configure(ctx, root, "cfg-course", courseGroup); e != nil {
		t.Fatal(e)
	}

	freshmanGroup := config()
	freshmanGroup.ID = "buaa-freshman-2026"
	freshmanGroup.School = "buaa"
	freshmanGroup.Name = "2026级新生迎新群"
	freshmanGroup.Mode = "password"
	freshmanGroup.RequireStudent = false
	freshmanGroup.Password = strptr("welcome-2026")
	freshmanGroup.Discoverable = true
	freshmanGroup.Category = "grade"
	freshmanGroup.Description = "凭录取通知书动态口令入群"
	freshmanGroup.PasswordHint = "口令见迎新通知单右下角"
	freshmanGroup.Number = strptr("1122334455")
	if _, e := f.s.Configure(ctx, root, "cfg-freshman", freshmanGroup); e != nil {
		t.Fatal(e)
	}

	hiddenGroup := config()
	hiddenGroup.ID = "buaa-hidden"
	hiddenGroup.School = "buaa"
	hiddenGroup.Name = "内部工作群"
	hiddenGroup.Discoverable = false
	hiddenGroup.Category = "department"
	if _, e := f.s.Configure(ctx, root, "cfg-hidden", hiddenGroup); e != nil {
		t.Fatal(e)
	}

	otherSchoolGroup := config()
	otherSchoolGroup.ID = "other-club"
	otherSchoolGroup.School = "other-school"
	otherSchoolGroup.Name = "外校社团群"
	otherSchoolGroup.Discoverable = true
	otherSchoolGroup.Category = "club"
	if _, e := f.s.Configure(ctx, root, "cfg-other", otherSchoolGroup); e != nil {
		t.Fatal(e)
	}

	// User applies to freshmanGroup and joins courseGroup; outsider joins otherSchoolGroup.
	if _, e := f.s.Command(ctx, user, "buaa-freshman-2026", "u-apply-fresh", g.Command{Kind: "apply"}); e != nil {
		t.Fatal(e)
	}
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		if e := tx.Put("buaa-course-os/members", user.Key(), g.Member{Owner: user.Key(), Joined: true, State: "restored"}); e != nil {
			return e
		}
		return tx.Put("buaa-course-os/members", outsider.Key(), g.Member{Owner: outsider.Key(), Joined: true, State: "grace"})
	}); e != nil {
		t.Fatal(e)
	}

	// PasswordHint appears on MyGroup when requires_password is true, and is omitted otherwise.
	myPage := personalPage(t, f, user, "")
	if len(myPage.Items) != 2 {
		t.Fatalf("unexpected my groups: %+v", myPage.Items)
	}
	for _, item := range myPage.Items {
		switch item.ID {
		case "buaa-course-os":
			if item.RequiresPassword || item.PasswordHint != "" {
				t.Fatalf("non-password group exposed hint: %+v", item)
			}
		case "buaa-freshman-2026":
			if !item.RequiresPassword || item.PasswordHint != "口令见迎新通知单右下角" {
				t.Fatalf("missing password_hint on personal view: %+v", item)
			}
		default:
			t.Fatalf("unexpected group: %+v", item)
		}
	}

	// Verified student in buaa sees both buaa discoverable groups, not hidden or other-school student groups.
	var factCalls atomic.Int64
	f.s.Facts = groupFactsFunc(func(_ context.Context, _ account.Principal, owner, school string) (g.Facts, error) {
		factCalls.Add(1)
		return g.Facts{Bound: true, StudentKnown: true, Student: owner == user.Key() && school == "buaa", StageKnown: true}, nil
	})
	before := databaseDigest(t, f)
	page := directoryPage(t, f, user, "")
	if len(page.Items) != 2 || page.NextCursor != "" || factCalls.Load() != 2 {
		t.Fatalf("items=%+v cursor=%q calls=%d", page.Items, page.NextCursor, factCalls.Load())
	}
	if page.Items[0].ID != "buaa-course-os" || page.Items[0].Number != "*****4321" || !page.Items[0].Joined || page.Items[0].State != "restored" || page.Items[0].PasswordHint != "" {
		t.Fatalf("unexpected course directory card: %+v", page.Items[0])
	}
	if page.Items[1].ID != "buaa-freshman-2026" || page.Items[1].Number != "******4455" || page.Items[1].Joined || page.Items[1].State != "grace" || page.Items[1].PasswordHint != "口令见迎新通知单右下角" {
		t.Fatalf("unexpected freshman directory card: %+v", page.Items[1])
	}
	raw, _ := json.Marshal(page)
	for _, secret := range []string{"987654321", "1122334455", "welcome-2026", "member_count", "pending_count", "bot_enabled", "buaa-hidden", "other-club"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("directory leaked %s: %s", secret, raw)
		}
	}
	if before != databaseDigest(t, f) {
		t.Fatal("directory GET mutated database")
	}

	// Unverified newcomer (Student=false) still discovers public non-student freshman group, not require_student groups.
	unverifiedPage := directoryPage(t, f, outsider, "?school=buaa")
	if len(unverifiedPage.Items) != 1 || unverifiedPage.Items[0].ID != "buaa-freshman-2026" || unverifiedPage.Items[0].Joined || unverifiedPage.Items[0].State != "" {
		t.Fatalf("unexpected unverified directory view: %+v", unverifiedPage)
	}

	// Category filter and cursor pagination work deterministically.
	byCategory := directoryPage(t, f, user, "?school=buaa&category=course")
	if len(byCategory.Items) != 1 || byCategory.Items[0].ID != "buaa-course-os" {
		t.Fatalf("category filter failed: %+v", byCategory)
	}
	p1 := directoryPage(t, f, user, "?school=buaa&limit=1")
	if len(p1.Items) != 1 || p1.Items[0].ID != "buaa-course-os" || p1.NextCursor != "buaa-course-os" {
		t.Fatalf("page 1: %+v", p1)
	}
	p2 := directoryPage(t, f, user, "?school=buaa&limit=1&cursor="+p1.NextCursor)
	if len(p2.Items) != 1 || p2.Items[0].ID != "buaa-freshman-2026" || p2.NextCursor != "buaa-freshman-2026" {
		t.Fatalf("page 2: %+v", p2)
	}
	p3 := directoryPage(t, f, user, "?school=buaa&limit=1&cursor="+p2.NextCursor)
	if len(p3.Items) != 0 || p3.NextCursor != "" {
		t.Fatalf("page 3: %+v", p3)
	}

	// Per-group blacklist hides that group; global blacklist returns 403.
	if _, e := f.s.Blacklists(ctx, root, "blk-group", g.BlacklistInput{Groups: []string{"buaa-course-os"}, Target: user.Key(), Reason: "spam"}); e != nil {
		t.Fatal(e)
	}
	afterGroupBlock := directoryPage(t, f, user, "?school=buaa")
	if len(afterGroupBlock.Items) != 1 || afterGroupBlock.Items[0].ID != "buaa-freshman-2026" {
		t.Fatalf("group blacklist did not hide group: %+v", afterGroupBlock)
	}
	if _, e := f.s.Blacklists(ctx, root, "blk-global", g.BlacklistInput{Groups: []string{"*"}, Target: outsider.Key(), Reason: "banned"}); e != nil {
		t.Fatal(e)
	}
	problem(t, api(t, f, outsider, "GET", "/api/v1/groups/directory", "", nil), 403, "groups_forbidden")
}

func TestGroupDirectoryValidationSDKAndFailureBoundaries(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}

	// Reserved route ID "directory" cannot be configured as a group.
	reserved := config()
	reserved.ID = "directory"
	problem(t, api(t, f, root, "PUT", "/api/v1/groups/directory", "reserved-dir", reserved), 422, "groups_unprocessable")

	// Legacy JSON configuration omitting optional B-01 fields remains valid and defaults to non-discoverable.
	legacyBody := map[string]any{
		"id": "legacy-json", "school": "school", "name": "兼容配置群", "mode": "before",
		"require_student": true, "accept_alumni": false, "accept_temporary": false,
		"grace_seconds": 3600, "reminder_seconds": 600, "retention_seconds": 0,
		"card_template": "", "auto_card": false, "check_card": false, "remind_card": false,
		"dice_sides": 0, "mute_probability": 0, "mute_seconds": 0, "cooldown_seconds": 0, "pity": 0,
		"keywords": []string{}, "repeat_threshold": 0, "auto_actions": []string{},
		"welcome": "", "rules_template": "", "block_links": false,
		"frequency_seconds": 0, "frequency_limit": 0, "warning_limit": 0,
		"bot_enabled": true, "expected_revision": 0,
	}
	w := api(t, f, root, "PUT", "/api/v1/groups/legacy-json", "legacy-ok", legacyBody)
	if w.Code != 200 {
		t.Fatalf("legacy config rejected: %d %s", w.Code, w.Body.String())
	}
	shape(t, "GroupConfig", w.Body.Bytes())

	// Invalid category, overlong hint/description, and invalid directory queries fail cleanly.
	badCategory := config()
	badCategory.ID = "bad-cat"
	badCategory.Category = "unknown"
	problem(t, api(t, f, root, "PUT", "/api/v1/groups/bad-cat", "bad-cat", badCategory), 422, "groups_unprocessable")

	for _, q := range []string{"?unknown=1", "?category=invalid", "?school=*", "?limit=0", "?limit=101", "?school=a&school=b", "?cursor=" + strings.Repeat("x", 129)} {
		problem(t, api(t, f, user, "GET", "/api/v1/groups/directory"+q, "", nil), 400, "groups_invalid_request")
	}
	problem(t, api(t, f, account.Principal{}, "GET", "/api/v1/groups/directory", "", nil), 401, "groups_unauthenticated")

	// Generated Go SDK client reaches GET /api/v1/groups/directory.
	disc := config()
	disc.ID = "sdk-group"
	disc.Discoverable = true
	disc.Category = "school"
	disc.Description = "全校通知群"
	if _, e := f.s.Configure(ctx, root, "sdk-cfg", disc); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return user, nil }))
	defer server.Close()
	client, e := sdk.NewClientWithResponses(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	school := "school"
	category := sdk.ListGroupDirectoryParamsCategory("school")
	res, e := client.ListGroupDirectoryWithResponse(ctx, &sdk.ListGroupDirectoryParams{School: &school, Category: &category})
	if e != nil || res.StatusCode() != 200 || res.JSON200 == nil || len(res.JSON200.Items) != 1 || res.JSON200.Items[0].Id != "sdk-group" {
		t.Fatalf("sdk response: %+v err=%v", res, e)
	}

	// Dependency failure fails closed with 503 and no partial page.
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		return g.Facts{}, errors.New("upstream-down")
	})
	failResp := api(t, f, user, "GET", "/api/v1/groups/directory", "", nil)
	problem(t, failResp, 503, "groups_unavailable")
	if strings.Contains(failResp.Body.String(), "upstream-down") || strings.Contains(failResp.Body.String(), "items") {
		t.Fatal("leaked error or partial page")
	}
}
