package groups_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-groups/sdk"
	"github.com/StuHelper/stuhelper-platform/account"
)

func TestPasswordEpochRotationProtectsJoinedMembersAndResetsUnjoinedApplicants(t *testing.T) {
	f := setup(t)
	veteran := account.Principal{Issuer: "test", Subject: "veteran"}
	applicant := account.Principal{Issuer: "test", Subject: "applicant"}

	// Veteran joined and restored under before mode.
	command(t, f, veteran, "vet-apply", g.Command{Kind: "apply"})
	if e := f.s.MemberEvent(ctx, "g", event("vet-join", "qq-bot", "g", "groups.member_joined", 1), veteran.Key()); e != nil {
		t.Fatal(e)
	}
	if m := member(t, f, "g", veteran); !m.Joined || m.State != "restored" {
		t.Fatalf("unexpected veteran initial state: %+v", m)
	}

	// Switch group to password mode (epoch 1): joined restored veteran remains eligible, applicant must submit password.
	c := config()
	c.Mode = "password"
	c.Password = strptr("pass-v1")
	cfg1, e := configureCurrent(t, f, manager, "pwd-v1", c)
	if e != nil || cfg1.PasswordEpoch != 1 {
		t.Fatalf("expected password_epoch=1, got %+v %v", cfg1, e)
	}
	if _, e := f.s.Sweep(ctx, "g", ""); e != nil {
		t.Fatal(e)
	}
	if m := member(t, f, "g", veteran); m.State != "restored" || !m.Deadline.IsZero() {
		t.Fatalf("veteran downgraded after password mode switch: %+v", m)
	}

	// Applicant applies and submits pass-v1 (epoch 1), but has NOT yet joined QQ group.
	command(t, f, applicant, "app-apply", g.Command{Kind: "apply"})
	res := command(t, f, applicant, "app-pass-v1", g.Command{Kind: "password", Text: "pass-v1"})
	if res.State != "restored" {
		t.Fatalf("expected applicant restored on pass-v1, got %+v", res)
	}
	if m := member(t, f, "g", applicant); m.Joined || m.PasswordEpoch != 1 {
		t.Fatalf("unexpected applicant after pass-v1: %+v", m)
	}

	// Updating non-password group settings (Password == nil) preserves password_epoch=1 and applicant eligibility.
	c.Password = nil
	c.Welcome = "欢迎新同学"
	cfgSame, e := configureCurrent(t, f, manager, "cfg-welcome", c)
	if e != nil || cfgSame.PasswordEpoch != 1 {
		t.Fatalf("unrelated config update changed password_epoch: %+v %v", cfgSame, e)
	}
	if item := personalPage(t, f, applicant, "").Items[0]; item.Eligibility != "eligible" {
		t.Fatalf("applicant lost eligibility on non-password config edit: %+v", item)
	}

	// Leak rotation: admin rotates password to pass-v2 -> password_epoch=2.
	// Veteran stays eligible, whereas unjoined applicant must re-verify with pass-v2!
	c.Password = strptr("pass-v2")
	cfg2, e := configureCurrent(t, f, manager, "pwd-v2", c)
	if e != nil || cfg2.PasswordEpoch != 2 {
		t.Fatalf("expected password_epoch=2, got %+v %v", cfg2, e)
	}

	vetView := personalPage(t, f, veteran, "").Items[0]
	if vetView.Eligibility != "eligible" || len(vetView.MissingConditions) != 0 {
		t.Fatalf("joined restored veteran affected by password rotation: %+v", vetView)
	}

	f.facts.set(false)
	if _, err := f.s.Sweep(ctx, "g", ""); err != nil {
		t.Fatal(err)
	}
	if old := member(t, f, "g", veteran); old.State != "grace" {
		t.Fatal(old)
	}
	f.facts.set(true)
	if _, err := f.s.Sweep(ctx, "g", ""); err != nil {
		t.Fatal(err)
	}
	if recovered := member(t, f, "g", veteran); recovered.State != "restored" || !recovered.Deadline.IsZero() {
		t.Fatal(recovered)
	}
	appView := personalPage(t, f, applicant, "").Items[0]
	if appView.Eligibility != "ineligible" || !slices.Contains(appView.MissingConditions, "password") || !slices.Contains(appView.NextActions, "submit_password") {
		t.Fatalf("unjoined applicant bypassed rotated password: %+v", appView)
	}
	if _, e := f.s.Command(ctx, applicant, "g", "app-old-pwd", g.Command{Kind: "password", Text: "pass-v1"}); !errors.Is(e, g.ErrForbidden) {
		t.Fatalf("old password still accepted after rotation: %v", e)
	}
	res2 := command(t, f, applicant, "app-pass-v2", g.Command{Kind: "password", Text: "pass-v2"})
	if res2.State != "restored" || member(t, f, "g", applicant).PasswordEpoch != 2 {
		t.Fatalf("new password did not advance epoch: %+v", res2)
	}
}

func TestSideEffectFreeCardPreviewBraceSafetyAndMyGroupMissingFields(t *testing.T) {
	f := setup(t)

	// Enable RemindCard and AutoCard so any side-effecting card check would queue 2 bot actions.
	c := config()
	c.RemindCard = true
	c.AutoCard = true
	c.CardTemplate = "{department}-{name}{separator}{nickname}"
	if _, e := configureCurrent(t, f, manager, "card-cfg", c); e != nil {
		t.Fatal(e)
	}

	// Invalid card template syntax is rejected at Configure time.
	badCfg := c
	badCfg.ExpectedRevision = 2
	badCfg.CardTemplate = "{name}-{unknown_token}"
	problem(t, api(t, f, manager, "PUT", "/api/v1/groups/g", "bad-tmpl", badCfg), 422, "groups_unprocessable")

	// Member has name, gender=male, and nickname containing braces & "{separator}", while department and student_id are undisclosed.
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		return g.Facts{
			Bound: true, StudentKnown: true, Student: true, StageKnown: true,
			Fields: map[string]g.CardField{
				"name":       {Allowed: true, Available: true, Value: "李四"},
				"gender":     {Allowed: true, Available: true, Value: "male"},
				"nickname":   {Allowed: true, Available: true, Value: "{C++}{separator}"},
				"department": {Allowed: false, Available: true, Value: "计算机学院"},
				"student_id": {Allowed: false, Available: false, Value: ""},
			},
		}, nil
	})
	putOwned(t, f, g.GroupConfig{GroupSettings: c.GroupSettings, Number: "*****6789", Revision: 2}, user, g.Member{Joined: true, State: "restored"})

	// 1. card_preview on stored template reports missing_fields=["department"] and queues ZERO bot actions.
	prevMissing := command(t, f, manager, "prev-missing", g.Command{Kind: "card_preview", Target: user.Key()})
	if prevMissing.State != "pending_profile" || !reflect.DeepEqual(prevMissing.MissingFields, []string{"department"}) || count(t, f, "g/actions") != 0 {
		t.Fatalf("unexpected missing card preview: %+v actions=%d", prevMissing, count(t, f, "g/actions"))
	}

	// 2. card_preview with candidate template override renders braces literally without secondary replacement or queuing actions.
	server := httptest.NewServer(httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return manager, nil }))
	defer server.Close()
	client, e := sdk.NewClientWithResponses(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	override := "{name}{separator}{nickname}"
	var previewCmd sdk.Command
	_ = previewCmd.FromCardPreviewCommand(sdk.CardPreviewCommand{Kind: "card_preview", Target: user.Key(), Text: &override})
	sdkRes, e := client.CommandWithResponse(ctx, "g", &sdk.CommandParams{IdempotencyKey: "sdk-card-prev"}, previewCmd)
	if e != nil || sdkRes.StatusCode() != http.StatusOK || sdkRes.JSON200 == nil {
		t.Fatalf("sdk card_preview failed: %+v %v", sdkRes, e)
	}
	if sdkRes.JSON200.State != "ready" || sdkRes.JSON200.Detail != "李四-{C++}{separator}" || count(t, f, "g/actions") != 0 {
		t.Fatalf("unexpected ready card_preview: %+v actions=%d", sdkRes.JSON200, count(t, f, "g/actions"))
	}

	// 3. Student MyGroup view exposes missing_card_fields=["department"] without leaking any field values.
	my := personalPage(t, f, user, "")
	if len(my.Items) != 1 || !reflect.DeepEqual(my.Items[0].MissingCardFields, []string{"department"}) {
		t.Fatalf("expected missing_card_fields=[department], got %+v", my.Items)
	}
	raw := api(t, f, user, "GET", "/api/v1/groups/me", "", nil).Body.String()
	for _, secret := range []string{"李四", "计算机学院", "{C++}"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("MyGroups leaked card field value %q: %s", secret, raw)
		}
	}
}
