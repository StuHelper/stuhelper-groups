package accountquery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type authStub struct {
	grants map[string][]Scope
	err    error
}

func (a authStub) Scopes(_ context.Context, _ Principal, c string) ([]Scope, error) {
	return a.grants[c], a.err
}

type repoStub struct {
	items  []Record
	called *bool
	err    error
}

func (r repoStub) ReadAccount(_ context.Context, _ Selection) ([]Record, error) {
	*r.called = true
	items := append([]Record{}, r.items...)
	for i := range items {
		items[i].Fields = append([]Field{}, items[i].Fields...)
	}
	return items, r.err
}

type auditStub struct {
	called *bool
	err    error
}

func (a auditStub) Record(_ context.Context, v Audit) error { *a.called = true; return a.err }
func TestAccountQueryAuthorizationAndFailures(t *testing.T) {
	ctx := context.Background()
	p := Principal{"issuer", "operator"}
	target := Principal{"issuer", "target"}
	scope := Scope{"school", "*", Domain}
	q := Query{Kind: "account", Value: target.Key(), Scope: scope, Purpose: "case", RequestID: "request", Limit: 1}
	called := false
	audited := false
	row := Record{ID: Domain + ":b", Subject: q.Value, Scope: scope, Source: Domain, Current: true, ChangedAt: time.Now(), Fields: []Field{{Name: "state", Category: "business", Value: "hidden"}}}
	svc := Service{Repo: repoStub{[]Record{row}, &called, nil}, Auth: authStub{map[string][]Scope{Capability: {scope}}, nil}}
	got, e := svc.Query(ctx, p, q)
	if e != nil || len(got) != 1 || !called {
		t.Fatal(got, e)
	}
	for _, grants := range []map[string][]Scope{{"other.account-query": {scope}}, {Capability: {{"other", "*", Domain}}}, {Capability: {{"school", "*", "other"}}}} {
		called = false
		svc.Auth = authStub{grants, nil}
		if _, e = svc.Query(ctx, p, q); !errors.Is(e, ErrForbidden) || called {
			t.Fatal("domain/scope bypass", e)
		}
	}
	svc.Auth = authStub{nil, errors.New("private dependency error")}
	if _, e = svc.Query(ctx, p, q); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	svc.Auth = authStub{map[string][]Scope{Capability: {scope}}, nil}
	svc.Repo = repoStub{nil, &called, errors.New(q.Value)}
	if _, e = svc.Query(ctx, p, q); e != ErrUnavailable {
		t.Fatal("repository error leaked", e)
	}
	svc.Repo = repoStub{[]Record{row}, &called, nil}
	q.After = row.ID
	if _, e = svc.Query(ctx, p, q); e != ErrUnavailable {
		t.Fatal("nonexclusive repository page accepted", e)
	}
	q.After = ""
	q.Limit = 101
	if _, e = svc.Query(ctx, p, q); e != ErrInvalid {
		t.Fatal(e)
	}
	q.Limit = 1
	for _, f := range []string{fmt.Sprint(q), fmt.Sprintf("%#v", q)} {
		if strings.Contains(f, q.Value) {
			t.Fatal("query in logs")
		}
	}
	if SensitiveCapability == "" {
		return
	}
	row.Fields = append(row.Fields, Field{Name: "sensitive", Category: "identity", Value: "secret"})
	svc.Repo = repoStub{[]Record{row}, &called, nil}
	got, e = svc.Query(ctx, p, q)
	if e != nil || got[0].Fields[1].Value != "***" || !got[0].Fields[1].Masked {
		t.Fatal(got, e)
	}
	svc.Audit = auditStub{&audited, nil}
	q.Fields = []string{"business", "identity"}
	if _, e = svc.Query(ctx, p, q); e != ErrForbidden {
		t.Fatal("sensitive capability not independent", e)
	}
	svc.Auth = authStub{map[string][]Scope{Capability: {scope}, SensitiveCapability: {scope}}, nil}
	svc.Audit = nil
	if _, e = svc.Query(ctx, p, q); e != ErrUnavailable {
		t.Fatal("missing audit accepted", e)
	}
	svc.Audit = auditStub{&audited, errors.New("offline")}
	if _, e = svc.Query(ctx, p, q); e != ErrUnavailable {
		t.Fatal(e)
	}
	svc.Audit = auditStub{&audited, nil}
	got, e = svc.Query(ctx, p, q)
	if e != nil || !audited || got[0].Fields[1].Value != "secret" {
		t.Fatal(got, e)
	}
}
