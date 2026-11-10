package groups_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-groups/sdk"
	"github.com/StuHelper/stuhelper-platform/account"
)

func myConfig(id, school string) g.GroupConfig {
	in := config()
	in.ID = id
	in.School = school
	return g.GroupConfig{GroupSettings: in.GroupSettings, Number: "*****6789", Revision: 1}
}
func putOwned(t *testing.T, f *fixture, c g.GroupConfig, owner account.Principal, m g.Member) {
	t.Helper()
	m.Owner = owner.Key()
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		if e := tx.Put("config", c.ID, c); e != nil {
			return e
		}
		return tx.Put(c.ID+"/members", owner.Key(), m)
	}); e != nil {
		t.Fatal(e)
	}
}
func personalPage(t *testing.T, f *fixture, p account.Principal, query string) g.Page[g.MyGroup] {
	t.Helper()
	w := api(t, f, p, "GET", "/api/v1/groups/me"+query, "", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	shape(t, "MyGroupPage", w.Body.Bytes())
	var out g.Page[g.MyGroup]
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	return out
}
func databaseDigest(t *testing.T, f *fixture) string {
	t.Helper()
	var digest string
	e := f.pool.QueryRow(ctx, `SELECT md5(COALESCE((SELECT string_agg(bucket||id||data::text,'' ORDER BY bucket,id) FROM groups.records),'')||COALESCE((SELECT string_agg(envelope::text,'' ORDER BY sequence) FROM groups.outbox),'')||COALESCE((SELECT string_agg(source||id||digest,'' ORDER BY source,id) FROM groups.inbox),''))`).Scan(&digest)
	if e != nil {
		t.Fatal(e)
	}
	return digest
}

func TestMyGroupsOwnIsolationPaginationAndReadOnly(t *testing.T) {
	f := setup(t)
	seedMembership(t, f, user, 52)
	deadline := f.now.Add(time.Hour)
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		for i := 0; i < 52; i++ {
			id := fmt.Sprintf("owned-%04d", i)
			if e := tx.Put(id+"/members", user.Key(), g.Member{Owner: user.Key(), Joined: i%2 == 0, State: "grace", Deadline: deadline}); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	putOwned(t, f, myConfig("other-user", "school"), outsider, g.Member{Joined: true, State: "restored"})
	otherIssuer := account.Principal{Issuer: "different", Subject: user.Subject}
	putOwned(t, f, myConfig("other-issuer", "school"), otherIssuer, g.Member{Joined: true, State: "restored"})
	putOwned(t, f, myConfig("removed", "school"), user, g.Member{Joined: true, State: "removed"})
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		return tx.Put("unmanaged/members", user.Key(), g.Member{Owner: user.Key(), Joined: true, State: "restored"})
	}); e != nil {
		t.Fatal(e)
	}
	// No management service is needed: /me authorizes ownership, not a role grant.
	f.s.Auth = nil
	var factCalls atomic.Int64
	f.s.Facts = groupFactsFunc(func(_ context.Context, _ account.Principal, owner, school string) (g.Facts, error) {
		if owner != user.Key() {
			return g.Facts{}, g.ErrForbidden
		}
		factCalls.Add(1)
		return g.Facts{Bound: true, StudentKnown: true, Student: true, StageKnown: true, Fields: map[string]g.CardField{"name": {Allowed: true, Available: true, Value: "private-member-name"}}}, nil
	})
	before := databaseDigest(t, f)
	first := personalPage(t, f, user, "")
	if len(first.Items) != 50 || first.NextCursor != "owned-0049" || factCalls.Load() != 2 {
		t.Fatal(len(first.Items), first.NextCursor, factCalls.Load())
	}
	for i, item := range first.Items {
		if item.ID != fmt.Sprintf("owned-%04d", i) || item.Joined != (i%2 == 0) || item.State != "grace" || item.Deadline == nil || !item.Deadline.Equal(deadline) {
			t.Fatalf("personal record lost: %+v", item)
		}
	}
	second := personalPage(t, f, user, "?cursor="+first.NextCursor+"&limit=100")
	if len(second.Items) != 2 || second.NextCursor != "" {
		t.Fatal(second)
	}
	all := personalPage(t, f, user, "?limit=100")
	if len(all.Items) != 52 || all.NextCursor != "" {
		t.Fatal(all)
	}
	raw, e := json.Marshal(all)
	if e != nil {
		t.Fatal(e)
	}
	for _, private := range []string{"other-user", "other-issuer", "removed", "unmanaged", "private-member-name", "member_count", "pending_count", "bot_enabled", "password_digest", user.Key()} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private or unrelated information leaked", private)
		}
	}
	if before != databaseDigest(t, f) {
		t.Fatal("personal GET mutated state, audit, inbox or outbox")
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=bad", "?limit=", "?limit=1&limit=2", "?cursor=" + strings.Repeat("a", 129), "?owner=" + outsider.Key(), "?issuer=other", "?subject=other"} {
		problem(t, api(t, f, user, "GET", "/api/v1/groups/me"+q, "", nil), 400, "groups_invalid_request")
	}
	problem(t, api(t, f, account.Principal{}, "GET", "/api/v1/groups/me", "", nil), 401, "groups_unauthenticated")
	nobody := account.Principal{Issuer: "test", Subject: "empty"}
	empty := personalPage(t, f, nobody, "")
	if len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatal(empty)
	}
	// Ordinary reverse lookup retains its joined-only semantics.
	joined, e := f.s.GroupsForOwner(ctx, user, "", 100)
	if e != nil || len(joined.Items) != 26 {
		t.Fatal(joined, e)
	}
}

func TestMyGroupsMissingConditionsAndUnknownFacts(t *testing.T) {
	f := setup(t)
	c := myConfig("g", "school")
	deadline := f.now.Add(time.Hour)
	m := g.Member{Joined: true, State: "grace", Deadline: deadline}
	cases := []struct {
		name                      string
		facts                     g.Facts
		eligibility               string
		missing, unknown, actions []string
	}{
		{"qq", g.Facts{StudentKnown: true, Student: true, StageKnown: true}, "ineligible", []string{"qq_binding"}, []string{}, []string{"bind_qq"}},
		{"unverified", g.Facts{Bound: true, StudentKnown: true}, "ineligible", []string{"student_verification"}, []string{}, []string{"verify_student"}},
		{"unknown-student", g.Facts{Bound: true}, "unknown", []string{}, []string{"student_eligibility"}, []string{"refresh"}},
		{"unknown-stage", g.Facts{Bound: true, StudentKnown: true, Student: true}, "unknown", []string{}, []string{"student_stage"}, []string{"refresh"}},
		{"alumni", g.Facts{Bound: true, StudentKnown: true, Student: true, StageKnown: true, Alumni: true}, "ineligible", []string{"alumni_not_accepted"}, []string{}, []string{"contact_group_admin"}},
		{"temporary", g.Facts{Bound: true, StudentKnown: true, Student: true, StageKnown: true, Temporary: true}, "ineligible", []string{"temporary_not_accepted"}, []string{}, []string{"contact_group_admin"}},
		{"known-and-unknown", g.Facts{}, "unknown", []string{"qq_binding"}, []string{"student_eligibility"}, []string{"bind_qq", "refresh"}},
		{"eligible", g.Facts{Bound: true, StudentKnown: true, Student: true, StageKnown: true}, "eligible", []string{}, []string{}, []string{"refresh"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			putOwned(t, f, c, user, m)
			f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) { return tc.facts, nil })
			before := databaseDigest(t, f)
			page := personalPage(t, f, user, "")
			if len(page.Items) != 1 {
				t.Fatal(page)
			}
			item := page.Items[0]
			if item.Eligibility != tc.eligibility || !reflect.DeepEqual(item.MissingConditions, tc.missing) || !reflect.DeepEqual(item.UnknownConditions, tc.unknown) || !reflect.DeepEqual(item.NextActions, tc.actions) {
				t.Fatalf("unexpected assessment: %+v", item)
			}
			if before != databaseDigest(t, f) {
				t.Fatal("read mutated eligibility")
			}
			if tc.eligibility != "eligible" {
				result, e := f.s.Command(ctx, manager, "g", fmt.Sprint("recheck-", i), g.Command{Kind: "recheck", Target: user.Key()})
				if e != nil || result.State == "restored" {
					t.Fatal("personal assessment disagrees with admission", result, e)
				}
			}
		})
	}
	// Eligibility facts can change between requests; no cross-request cache persists.
	c.AcceptAlumni = true
	putOwned(t, f, c, user, m)
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		return g.Facts{Bound: true, StudentKnown: true, Student: true, StageKnown: true, Alumni: true}, nil
	})
	if personalPage(t, f, user, "").Items[0].Eligibility != "eligible" {
		t.Fatal("accepted alumni rejected")
	}
}

func TestMyGroupsDependencyFailureReturnsNoPartialPage(t *testing.T) {
	f := setup(t)
	seedMembership(t, f, user, 2)
	var calls int
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		calls++
		if calls == 2 {
			return g.Facts{Bound: true, StudentKnown: true, Student: true, StageKnown: true}, errors.New("private-diagnostic")
		}
		return g.Facts{Bound: true, StudentKnown: true, Student: true, StageKnown: true}, nil
	})
	before := databaseDigest(t, f)
	w := api(t, f, user, "GET", "/api/v1/groups/me", "", nil)
	problem(t, w, 503, "groups_unavailable")
	if strings.Contains(w.Body.String(), "items") || strings.Contains(w.Body.String(), "private-diagnostic") || before != databaseDigest(t, f) {
		t.Fatal("partial response or data mutation")
	}
	f.s.Facts = nil
	problem(t, api(t, f, user, "GET", "/api/v1/groups/me", "", nil), 503, "groups_unavailable")
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		return g.Facts{Alumni: true}, nil
	})
	problem(t, api(t, f, user, "GET", "/api/v1/groups/me", "", nil), 503, "groups_unavailable")
	f.s.Repo = nil
	problem(t, api(t, f, user, "GET", "/api/v1/groups/me", "", nil), 503, "groups_unavailable")
}

func TestMyGroupsPasswordRestrictionAndPublicProjection(t *testing.T) {
	f := setup(t)
	c := myConfig("g", "school")
	c.Mode = "password"
	c.PasswordSet = true
	c.Number = "123456789"
	m := g.Member{State: "awaiting_verification"}
	putOwned(t, f, c, user, m)
	page := personalPage(t, f, user, "")
	item := page.Items[0]
	if item.Number != "*****6789" || !item.RequiresPassword || item.Deadline != nil || item.Joined || item.Eligibility != "ineligible" || !slices.Contains(item.MissingConditions, "password") || !slices.Contains(item.NextActions, "submit_password") {
		t.Fatal(item)
	}
	m.Password = true
	putOwned(t, f, c, user, m)
	item = personalPage(t, f, user, "").Items[0]
	if item.Eligibility != "eligible" || len(item.MissingConditions) != 0 || !item.RequiresPassword {
		t.Fatal(item)
	}
	f.facts.set(false)
	item = personalPage(t, f, user, "").Items[0]
	if item.Eligibility != "ineligible" || !slices.Contains(item.MissingConditions, "student_verification") {
		t.Fatal("password granted student eligibility", item)
	}
	c.RequireStudent = false
	putOwned(t, f, c, user, m)
	item = personalPage(t, f, user, "").Items[0]
	if item.Eligibility != "eligible" {
		t.Fatal("optional student requirement applied", item)
	}
	if e := f.r.Transaction(ctx, func(tx g.Tx) error {
		return tx.Put("*/blacklist", user.Key(), g.Blacklist{Reason: "private-management-reason", Permanent: true})
	}); e != nil {
		t.Fatal(e)
	}
	item = personalPage(t, f, user, "").Items[0]
	if item.Eligibility != "ineligible" || !reflect.DeepEqual(item.MissingConditions, []string{"restricted"}) || !reflect.DeepEqual(item.NextActions, []string{"contact_group_admin"}) {
		t.Fatal(item)
	}
	raw, _ := json.Marshal(item)
	if strings.Contains(string(raw), "private-management-reason") {
		t.Fatal("reason leaked")
	}
}

func TestMyGroupsGeneratedClientAndRouteReservation(t *testing.T) {
	f := setup(t)
	f.facts.set(false)
	command(t, f, user, "apply", g.Command{Kind: "apply"})
	server := httptest.NewServer(httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return user, nil }))
	defer server.Close()
	client, e := sdk.NewClientWithResponses(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	response, e := client.ListMyGroupsWithResponse(ctx, nil)
	if e != nil || response.StatusCode() != 200 || response.JSON200 == nil || len(response.JSON200.Items) != 1 || response.JSON200.Items[0].Id != "g" {
		t.Fatal(response, e)
	}
	w := api(t, f, user, "GET", "/api/v1/groups", "", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var managed g.Page[g.GroupSummary]
	if e = json.Unmarshal(w.Body.Bytes(), &managed); e != nil || len(managed.Items) != 0 {
		t.Fatal("management list disclosed a group", managed, e)
	}
	root := account.Principal{Issuer: "test", Subject: "root"}
	input := config()
	input.ID = "me"
	problem(t, api(t, f, root, "PUT", "/api/v1/groups/me", "reserved", input), 422, "groups_unprocessable")
}
