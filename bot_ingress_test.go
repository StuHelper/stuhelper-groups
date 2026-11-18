package groups_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
)

func TestBotIngressObjectStreamsAndRecalledAttachments(t *testing.T) {
	f := setup(t)
	other := account.Principal{Issuer: "test", Subject: "other-member"}

	// Message m1 arrives with platform message sequence 1000 and two image attachments.
	msg1 := g.Message{
		ID:          "m1",
		Owner:       user.Key(),
		Body:        "recalled image caption",
		Attachments: []string{"media-img-1", "media-img-2"},
	}
	if err := f.s.Ingest(ctx, "g", event("ev-m1", "qq-bot", "g", "groups.message", 1000), msg1); err != nil {
		t.Fatal(err)
	}

	// Concurrent/out-of-order message m2 with lower sequence 500 must not be silently dropped.
	msg2 := g.Message{
		ID:          "m2",
		Owner:       other.Key(),
		Body:        "another message",
		Attachments: []string{"media-img-3"},
	}
	if err := f.s.Ingest(ctx, "g", event("ev-m2", "qq-bot", "g/messages/m2", "groups.message", 500), msg2); err != nil {
		t.Fatal(err)
	}

	// Member join events use independent sequence numbers starting at 1; must not be dropped by message rev 1000.
	if err := f.s.MemberEvent(ctx, "g", event("ev-join-user", "qq-bot", "g", "groups.member_joined", 1), user.Key()); err != nil {
		t.Fatal(err)
	}
	uMember, err := f.s.Progress(ctx, user, "g")
	if err != nil || !uMember.Joined || uMember.State != "restored" {
		t.Fatalf("member join dropped by message stream revision: member=%+v err=%v", uMember, err)
	}

	// Member leave at rev 5 followed by another member's join at rev 1 must not interfere across members.
	if err := f.s.MemberEvent(ctx, "g", event("ev-join-other", "qq-bot", "g/members/"+other.Key(), "groups.member_joined", 5), other.Key()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MemberEvent(ctx, "g", event("ev-left-user", "qq-bot", "g", "groups.member_left", 2), user.Key()); err != nil {
		t.Fatal(err)
	}
	uMember, err = f.s.Progress(ctx, user, "g")
	if err != nil || uMember.Joined || uMember.State != "removed" {
		t.Fatalf("member leave dropped by another member's revision: member=%+v err=%v", uMember, err)
	}

	// Stale revision on the same member stream is ignored without reverting state.
	if err := f.s.MemberEvent(ctx, "g", event("ev-stale-join-user", "qq-bot", "g", "groups.member_joined", 1), user.Key()); err != nil {
		t.Fatal(err)
	}
	uMember, err = f.s.Progress(ctx, user, "g")
	if err != nil || uMember.Joined || uMember.State != "removed" {
		t.Fatalf("stale member event overwrote newer state: member=%+v err=%v", uMember, err)
	}

	// Recall notification from bot adapter (e.g. moderator recall) omits original body and attachments.
	recall1 := g.Message{
		ID:       "m1",
		Owner:    manager.Key(),
		Recalled: true,
	}
	if err := f.s.Ingest(ctx, "g", event("ev-m1-recall", "qq-bot", "g", "groups.message", 1001), recall1); err != nil {
		t.Fatal(err)
	}

	auditBefore := count(t, f, "g/audit")
	page, err := f.s.Messages(ctx, manager, "g", "", 10)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("expected 2 retained messages, got %+v err=%v", page, err)
	}
	if count(t, f, "g/audit") != auditBefore {
		t.Fatal("viewing recalled messages wrote business audit")
	}
	gotM1 := page.Items[0]
	if gotM1.ID != "m1" || !gotM1.Recalled || gotM1.Owner != user.Key() || gotM1.Body != "recalled image caption" || !reflect.DeepEqual(gotM1.Attachments, []string{"media-img-1", "media-img-2"}) {
		t.Fatalf("recalled message lost attachments or original owner: %+v", gotM1)
	}

	// Same event ID with different subject or payload must return ErrConflict.
	if err := f.s.Ingest(ctx, "g", event("ev-m1-recall", "qq-bot", "g/messages/m1", "groups.message", 1001), recall1); !errors.Is(err, g.ErrConflict) {
		t.Fatalf("expected ErrConflict on envelope subject mutation, got %v", err)
	}

	// Once retention window expires, late recall must not revive message or emit message.changed.
	var outboxBefore int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.outbox WHERE envelope->>'type'='groups.message.changed'").Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(121 * time.Second)
	if err := f.s.Ingest(ctx, "g", event("ev-m2-expired-recall", "qq-bot", "g", "groups.message", 501), g.Message{ID: "m2", Owner: other.Key(), Recalled: true}); err != nil {
		t.Fatal(err)
	}
	var outboxAfter int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM groups.outbox WHERE envelope->>'type'='groups.message.changed'").Scan(&outboxAfter); err != nil {
		t.Fatal(err)
	}
	if outboxAfter != outboxBefore {
		t.Fatalf("expired message recall emitted message.changed: %d -> %d", outboxBefore, outboxAfter)
	}
	afterExpiry, err := f.s.Messages(ctx, manager, "g", "", 10)
	if err != nil || len(afterExpiry.Items) != 0 {
		t.Fatalf("expired message exposed: %+v err=%v", afterExpiry, err)
	}
}

func TestBotIngressReplaysLegacyInbox(t *testing.T) {
	f := setup(t)
	ev := event("legacy", "qq-bot", "g", "groups.message", 9)
	msg := g.Message{ID: "m", Owner: user.Key(), Attachments: []string{}}
	data, err := json.Marshal(struct {
		EventData json.RawMessage
		Message   g.Message
	}{ev.Data, msg})
	if err != nil {
		t.Fatal(err)
	}
	legacy := ev
	legacy.Data = data
	if err = f.r.Transaction(ctx, func(tx g.Tx) error { _, err := tx.Accept(legacy); return err }); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Ingest(ctx, "g", ev, msg); err != nil || count(t, f, "g/messages") != 0 {
		t.Fatal("legacy event was not recognized", err)
	}
	msg.Body = "different"
	if err = f.s.Ingest(ctx, "g", ev, msg); !errors.Is(err, g.ErrConflict) {
		t.Fatal("changed legacy event accepted", err)
	}
}
