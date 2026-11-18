package groups_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-platform/account"
)

type tracingBot struct {
	mu           sync.Mutex
	execTraces   map[string]string
	lookupTraces map[string]string
	timeoutFirst map[string]bool
	out          map[string]g.Outcome
}

func (b *tracingBot) Execute(c context.Context, a g.Action) (g.Outcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.execTraces[a.ID] = g.TraceParentFromContext(c)
	outcome := g.Outcome{Success: true, Code: "ok"}
	b.out[a.ID] = outcome
	if b.timeoutFirst[a.ID] {
		delete(b.timeoutFirst, a.ID)
		return g.Outcome{}, context.DeadlineExceeded
	}
	return outcome, nil
}

func (b *tracingBot) Lookup(c context.Context, id string) (g.Outcome, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lookupTraces[id] = g.TraceParentFromContext(c)
	v, ok := b.out[id]
	return v, ok, nil
}

func httpRequestWithHeaders(t *testing.T, f *fixture, p account.Principal, method, path string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return p, nil }).ServeHTTP(w, req)
	return w
}

func drainPendingOutbox(t *testing.T, f *fixture) []g.Event {
	t.Helper()
	events, err := f.r.PendingOutbox(ctx, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if err := f.r.ConfirmOutbox(ctx, ev.ID); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

func TestTraceParentPropagationAcrossHTTPOutboxBotFanoutAndUpstream(t *testing.T) {
	f := setup(t)
	tb := &tracingBot{
		execTraces:   map[string]string{},
		lookupTraces: map[string]string{},
		timeoutFirst: map[string]bool{},
		out:          map[string]g.Outcome{},
	}
	f.s.Bot = tb
	_ = drainPendingOutbox(t, f)

	root := account.Principal{Issuer: "test", Subject: "root"}
	const tpHTTP = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	// 1. Configure receiver group via HTTP with Traceparent and subscribe it to "g".
	recvCfg := config()
	recvCfg.ID = "recv"
	w := httpRequestWithHeaders(t, f, root, "PUT", "/api/v1/groups/recv", map[string]string{
		"Idempotency-Key": "cfg-recv",
		"Traceparent":     tpHTTP,
	}, recvCfg)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	events := drainPendingOutbox(t, f)
	if len(events) != 1 || events[0].Type != "groups.configured" || events[0].TraceParent != tpHTTP {
		t.Fatalf("expected groups.configured with traceparent %q, got %+v", tpHTTP, events)
	}

	w = httpRequestWithHeaders(t, f, root, "POST", "/api/v1/groups/g/commands", map[string]string{
		"Idempotency-Key": "sub-recv",
		"Traceparent":     tpHTTP,
	}, g.Command{Kind: "subscribe", Receiver: "recv", Enabled: true})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	// 2. Queue a warn action via HTTP with Traceparent, trigger unknown->Lookup->succeeded, then Fanout.
	w = httpRequestWithHeaders(t, f, manager, "POST", "/api/v1/groups/g/commands", map[string]string{
		"Idempotency-Key": "warn-trace",
		"Traceparent":     tpHTTP,
	}, g.Command{Kind: "warn", Target: user.Key(), Text: "trace check"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var warnRes g.Result
	if err := json.Unmarshal(w.Body.Bytes(), &warnRes); err != nil {
		t.Fatal(err)
	}

	events = drainPendingOutbox(t, f)
	if len(events) != 1 || events[0].Type != "groups.action.requested" || events[0].TraceParent != tpHTTP {
		t.Fatalf("expected groups.action.requested with traceparent %q, got %+v", tpHTTP, events)
	}

	// Simulate first Execute timing out with bare background context: stored traceparent must still reach Bot.Execute and survive unknown state.
	tb.timeoutFirst[warnRes.ID] = true
	act, err := f.s.Execute(context.Background(), "g", warnRes.ID)
	if !errors.Is(err, context.DeadlineExceeded) || act.State != "unknown" {
		t.Fatalf("expected unknown state on timeout, got %+v err=%v", act, err)
	}
	if tb.execTraces[warnRes.ID] != tpHTTP {
		t.Fatalf("expected Bot.Execute traceparent %q, got %q", tpHTTP, tb.execTraces[warnRes.ID])
	}
	unknownEvents := drainPendingOutbox(t, f)
	if len(unknownEvents) != 1 || unknownEvents[0].Type != "groups.action.result" || unknownEvents[0].TraceParent != tpHTTP {
		t.Fatalf("expected unknown action.result with traceparent %q, got %+v", tpHTTP, unknownEvents)
	}

	// Second Execute resolves via Lookup even with bare context, then cleans up action_traces.
	act, err = f.s.Execute(context.Background(), "g", warnRes.ID)
	if err != nil || act.State != "succeeded" {
		t.Fatalf("expected succeeded on Lookup recovery, got %+v err=%v", act, err)
	}
	if tb.lookupTraces[warnRes.ID] != tpHTTP {
		t.Fatalf("expected Bot.Lookup traceparent %q, got %q", tpHTTP, tb.lookupTraces[warnRes.ID])
	}
	if count(t, f, "g/action_traces") != 0 {
		t.Fatalf("expected action_traces cleaned up after terminal completion, got %d", count(t, f, "g/action_traces"))
	}

	resultEvents := drainPendingOutbox(t, f)
	if len(resultEvents) != 1 || resultEvents[0].Type != "groups.action.result" || resultEvents[0].TraceParent != tpHTTP {
		t.Fatalf("expected succeeded action.result with traceparent %q, got %+v", tpHTTP, resultEvents)
	}

	// Fanout consumes action.result and propagates traceparent to subscriber send action.
	if err := f.s.Fanout(context.Background(), resultEvents[0]); err != nil {
		t.Fatal(err)
	}
	fanoutEvents := drainPendingOutbox(t, f)
	if len(fanoutEvents) != 1 || fanoutEvents[0].Subject != "recv" || fanoutEvents[0].TraceParent != tpHTTP {
		t.Fatalf("expected fanout outbox event with traceparent %q, got %+v", tpHTTP, fanoutEvents)
	}
	var fanoutPayload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(fanoutEvents[0].Data, &fanoutPayload); err != nil {
		t.Fatal(err)
	}
	sendAct, err := f.s.Execute(context.Background(), "recv", fanoutPayload.ID)
	if err != nil || sendAct.State != "succeeded" || tb.execTraces[fanoutPayload.ID] != tpHTTP {
		t.Fatalf("expected fanout send action to preserve traceparent %q, got state=%s trace=%q err=%v", tpHTTP, sendAct.State, tb.execTraces[fanoutPayload.ID], err)
	}
	_ = drainPendingOutbox(t, f)

	// 3. Upstream Consume propagates TraceParent to admission outbox events and rejects malformed TraceParent.
	f.facts.set(false)
	command(t, f, user, "apply-upstream", g.Command{Kind: "apply"})
	_ = drainPendingOutbox(t, f)

	const tpUpstream = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	f.facts.set(true)
	upstream := g.UpstreamEvent{
		SpecVersion:     "1.0",
		ID:              "ver-trace-1",
		Source:          "verification",
		Type:            "verification.credential.approved.v1",
		Subject:         "credential/1",
		TraceParent:     tpUpstream,
		Time:            f.now,
		DataContentType: "application/json",
		SchoolID:        "school",
		Data:            g.EligibilityChange{Owner: user.Key(), Revision: 1},
	}
	if err := f.s.Consume(context.Background(), upstream); err != nil {
		t.Fatal(err)
	}
	consumeEvents := drainPendingOutbox(t, f)
	if len(consumeEvents) == 0 {
		t.Fatal("expected admission action event from Consume")
	}
	for _, ev := range consumeEvents {
		if ev.TraceParent != tpUpstream {
			t.Fatalf("expected upstream traceparent %q, got %+v", tpUpstream, ev)
		}
	}

	badUpstream := upstream
	badUpstream.ID = "ver-trace-bad"
	badUpstream.Data.Revision = 2
	badUpstream.TraceParent = "bad traceparent\nwith-control"
	if err := f.s.Consume(context.Background(), badUpstream); !errors.Is(err, g.ErrInvalid) {
		t.Fatalf("expected ErrInvalid for malformed upstream traceparent, got %v", err)
	}

	// 4. Malformed HTTP Traceparent header with control/space characters is sanitized without failing valid requests.
	w = httpRequestWithHeaders(t, f, manager, "POST", "/api/v1/groups/g/commands", map[string]string{
		"Idempotency-Key": "send-bad-tp",
		"Traceparent":     "invalid header with spaces",
	}, g.Command{Kind: "send", Text: "hello"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	sanitizedEvents := drainPendingOutbox(t, f)
	if len(sanitizedEvents) != 1 || sanitizedEvents[0].TraceParent != "" {
		t.Fatalf("expected invalid HTTP Traceparent to be omitted, got %+v", sanitizedEvents)
	}
}

func TestTypedCollectionDecodingAndGenericPageRoutes(t *testing.T) {
	f := setup(t)
	root := account.Principal{Issuer: "test", Subject: "root"}
	recvCfg := config()
	recvCfg.ID = "recv"
	if _, err := f.s.Configure(ctx, root, "cfg-recv-list", recvCfg); err != nil {
		t.Fatal(err)
	}

	// Seed data across all 10 group collections.
	command(t, f, user, "apply-list", g.Command{Kind: "apply"})
	if err := f.s.Ingest(ctx, "g", g.Event{
		SpecVersion: "1.0",
		ID:          "msg-evt-1",
		Source:      "qq-bot",
		Type:        "groups.message",
		Subject:     "g",
		Time:        f.now,
		Revision:    1,
		Data:        json.RawMessage(`{}`),
	}, g.Message{ID: "m1", Owner: user.Key(), Body: "spam link http://example.com", Attachments: []string{"att-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MemberEvent(ctx, "g", event("reporter-joined", "qq-bot", "g", "groups.member_joined", 1), user.Key()); err != nil {
		t.Fatal(err)
	}
	command(t, f, user, "report-1", g.Command{Kind: "report", Message: "m1", Text: "spamming"})
	command(t, f, manager, "rule-1", g.Command{Kind: "rule", Target: user.Key(), Message: "m1", Text: "spam"})
	if _, err := f.s.Command(ctx, root, "g", "sub-1", g.Command{Kind: "subscribe", Receiver: "recv", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	command(t, f, manager, "bl-1", g.Command{Kind: "blacklist", Target: outsider.Key(), Text: "abuse", Seconds: 600})
	command(t, f, manager, "watch-1", g.Command{Kind: "watch", Target: user.Key(), Enabled: true, Text: "observe"})
	warnRes := command(t, f, manager, "warn-1", g.Command{Kind: "warn", Target: user.Key(), Text: "first warning"})
	if _, err := f.s.Execute(ctx, "g", warnRes.ID); err != nil {
		t.Fatal(err)
	}
	events, err := f.r.PendingOutbox(ctx, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == "groups.action.result" && ev.Subject == "g" {
			if err := f.s.Fanout(ctx, ev); err != nil {
				t.Fatal(err)
			}
		}
	}

	routes := []struct {
		path   string
		schema string
	}{
		{"/api/v1/groups/g/members", "MemberPage"},
		{"/api/v1/groups/g/actions", "ActionPage"},
		{"/api/v1/groups/g/reports", "ReportPage"},
		{"/api/v1/groups/g/hits", "HitPage"},
		{"/api/v1/groups/g/messages", "MessagePage"},
		{"/api/v1/groups/g/subscriptions", "SubscriptionPage"},
		{"/api/v1/groups/g/blacklist", "BlacklistPage"},
		{"/api/v1/groups/g/warnings", "WarningsPage"},
		{"/api/v1/groups/g/watch", "WatchPage"},
		{"/api/v1/groups/g/deliveries", "DeliveryPage"},
	}
	for _, rt := range routes {
		w := api(t, f, manager, "GET", rt.path+"?limit=10", "", nil)
		if w.Code != 200 {
			t.Fatalf("%s returned %d: %s", rt.path, w.Code, w.Body.String())
		}
		shape(t, rt.schema, w.Body.Bytes())
		var page struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 0 {
			t.Fatalf("expected non-empty items for %s", rt.path)
		}
		id, _ := page.Items[0]["id"].(string)
		if strings.TrimSpace(id) == "" {
			t.Fatalf("expected populated id on %s, got %+v", rt.path, page.Items[0])
		}
		problem(t, api(t, f, manager, "GET", rt.path+"?limit=0", "", nil), 400, "groups_invalid_request")
	}

	// Verify Delivery.State matches recipient Action.State ("pending" before Execute, "succeeded" after Execute).
	deliveries, err := f.s.Deliveries(ctx, manager, "g", "", 10)
	if err != nil || len(deliveries.Items) != 1 || deliveries.Items[0].State != "pending" {
		t.Fatalf("expected pending delivery state, got %+v err=%v", deliveries, err)
	}
	if _, err := f.s.Execute(ctx, "recv", deliveries.Items[0].Action); err != nil {
		t.Fatal(err)
	}
	deliveries, err = f.s.Deliveries(ctx, manager, "g", "", 10)
	if err != nil || len(deliveries.Items) != 1 || deliveries.Items[0].State != "succeeded" {
		t.Fatalf("expected succeeded delivery state, got %+v err=%v", deliveries, err)
	}

	// Verify expired messages are filtered out while single-item scan still advances NextCursor.
	f.now = f.now.Add(10 * time.Minute)
	msgs, err := f.s.Messages(ctx, manager, "g", "", 1)
	if err != nil || len(msgs.Items) != 0 || msgs.NextCursor != "m1" {
		t.Fatalf("expected expired message filtered with next_cursor=m1, got %+v err=%v", msgs, err)
	}
}
