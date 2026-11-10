package groups_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
)

// Generated from the Envelope/EventData tags and PendingOutbox assignments at
// qq-binding 49df06ddf58a4521ccb07ff61442e0b5ded6fce2; provenance is in testdata/README.md.
//
//go:embed testdata/qq-binding-owner-bindings.changed.json
var qqBindingEnvelopeJSON []byte

func qqBindingEnvelope(t *testing.T) g.UpstreamEvent {
	t.Helper()
	var e g.UpstreamEvent
	if err := json.Unmarshal(qqBindingEnvelopeJSON, &e); err != nil {
		t.Fatal(err)
	}
	if e.Type != "qq-binding.owner-bindings.changed.v1" || e.Source != "qq-binding" || e.Subject != user.Key() || e.Data.Owner != e.Subject || e.ID != "42" || e.Data.Revision != 42 {
		t.Fatal("provider fixture changed", e)
	}
	return e
}

func TestQQBindingActualEnvelopeRecomputeReplayAndOldRevision(t *testing.T) {
	f := setup(t)
	seedMembership(t, f, user, 2)
	var calls atomic.Int64
	var bound atomic.Bool
	f.s.Facts = groupFactsFunc(func(context.Context, account.Principal, string, string) (g.Facts, error) {
		calls.Add(1)
		return g.Facts{Bound: bound.Load(), StudentKnown: true, Student: true, StageKnown: true}, nil
	})
	event := qqBindingEnvelope(t)
	// Concurrent delivery of the actual provider shape must have one effect.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- f.s.Consume(ctx, event) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 || inboxCount(t, f, event.ID) != 1 {
		t.Fatal("duplicate recomputation", calls.Load())
	}
	for _, id := range []string{"owned-0000", "owned-0001"} {
		m := member(t, f, id, user)
		if m.State != "grace" || !m.Deadline.Equal(f.now.Add(time.Hour)) || count(t, f, id+"/actions") != 1 {
			t.Fatalf("binding change did not trigger admission: %+v", m)
		}
	}
	if err := f.s.Consume(ctx, event); err != nil || calls.Load() != 2 {
		t.Fatal("repeat executed", err)
	}
	older := event
	older.ID = "41"
	older.Data.Revision = 41
	if err := f.s.Consume(ctx, older); err != nil || calls.Load() != 2 || inboxCount(t, f, older.ID) != 1 {
		t.Fatal("old revision executed", err)
	}
	altered := event
	altered.Time = event.Time.Add(time.Second)
	if err := f.s.Consume(ctx, altered); !errors.Is(err, g.ErrConflict) {
		t.Fatal("same ID content conflict lost", err)
	}
	bound.Store(true)
	newer := event
	newer.ID = "43"
	newer.Data.Revision = 43
	if err := f.s.Consume(ctx, newer); err != nil || calls.Load() != 4 {
		t.Fatal(err, calls.Load())
	}
	for _, id := range []string{"owned-0000", "owned-0001"} {
		m := member(t, f, id, user)
		if m.State != "restored" || !m.Deadline.IsZero() {
			t.Fatal("current binding restoration ignored", m)
		}
	}
	// A lower revision in another owner's subject is independent of this stream.
	seedMembership(t, f, outsider, 1)
	bound.Store(false)
	other := event
	other.ID = "40"
	other.Data.Revision = 40
	other.Subject = outsider.Key()
	other.Data.Owner = outsider.Key()
	if err := f.s.Consume(ctx, other); err != nil || calls.Load() != 5 || member(t, f, "owned-0000", outsider).State != "grace" {
		t.Fatal("owner streams conflated", err)
	}
	// Keep full int64 precision, including revisions beyond JavaScript's safe integer range.
	large := event
	large.Data.Revision = 9007199254740993
	large.ID = strconv.FormatInt(large.Data.Revision, 10)
	raw, err := json.Marshal(large)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip g.UpstreamEvent
	if err = json.Unmarshal(raw, &roundtrip); err != nil || roundtrip.Data.Revision != large.Data.Revision {
		t.Fatal(err)
	}
	if err = f.s.Consume(ctx, roundtrip); err != nil || calls.Load() != 7 {
		t.Fatal("large provider revision rejected", err)
	}
}

func TestQQBindingEnvelopeValidationRejectsUnknownAndInconsistentFields(t *testing.T) {
	f := setup(t)
	seedMembership(t, f, user, 1)
	event := qqBindingEnvelope(t)
	before := databaseDigest(t, f)
	cases := map[string]func(*g.UpstreamEvent){
		"empty-binding-subtype":    func(e *g.UpstreamEvent) { e.Type = "qq-binding.binding.v1" },
		"empty-credential-subtype": func(e *g.UpstreamEvent) { e.Source = "verification"; e.Type = "verification.credential.v1" },
		"unknown-type":             func(e *g.UpstreamEvent) { e.Type = "qq-binding.owner-bindings.unknown.v1" },
		"unknown-version":          func(e *g.UpstreamEvent) { e.Type = "qq-binding.owner-bindings.changed.v2" },
		"wrong-source":             func(e *g.UpstreamEvent) { e.Source = "verification" },
		"wrong-subject":            func(e *g.UpstreamEvent) { e.Subject = outsider.Key() },
		"wrong-owner":              func(e *g.UpstreamEvent) { e.Data.Owner = outsider.Key() },
		"revision-mismatch":        func(e *g.UpstreamEvent) { e.Data.Revision = 43 },
		"zero-revision":            func(e *g.UpstreamEvent) { e.ID = "0"; e.Data.Revision = 0 },
		"negative-revision":        func(e *g.UpstreamEvent) { e.ID = "-1"; e.Data.Revision = -1 },
		"leading-zero":             func(e *g.UpstreamEvent) { e.ID = "042" },
		"signed-id":                func(e *g.UpstreamEvent) { e.ID = "+42" },
		"non-numeric-id":           func(e *g.UpstreamEvent) { e.ID = "event-42" },
		"missing-content-type":     func(e *g.UpstreamEvent) { e.DataContentType = "" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			bad := event
			change(&bad)
			if err := f.s.Consume(ctx, bad); !errors.Is(err, g.ErrInvalid) {
				t.Fatal(err)
			}
			if inboxCount(t, f, bad.ID) != 0 || databaseDigest(t, f) != before {
				t.Fatal("invalid envelope caused writes")
			}
		})
	}
	// The actual JSON field is numeric data.revision, not version or a quoted number.
	for _, raw := range []string{strings.Replace(string(qqBindingEnvelopeJSON), `"revision": 42`, `"version": 42`, 1), strings.Replace(string(qqBindingEnvelopeJSON), `"owner": "dGVzdA:dXNlcg"`, `"account": "dGVzdA:dXNlcg"`, 1)} {
		var bad g.UpstreamEvent
		if err := json.Unmarshal([]byte(raw), &bad); err != nil {
			t.Fatal(err)
		}
		if err := f.s.Consume(ctx, bad); !errors.Is(err, g.ErrInvalid) {
			t.Fatal("wrong data field accepted", err)
		}
	}
	quoted := strings.Replace(string(qqBindingEnvelopeJSON), `"revision": 42`, `"revision": "42"`, 1)
	var bad g.UpstreamEvent
	if err := json.Unmarshal([]byte(quoted), &bad); err == nil {
		t.Fatal("quoted revision accepted")
	}
	if databaseDigest(t, f) != before {
		t.Fatal("malformed payload caused writes")
	}
}
