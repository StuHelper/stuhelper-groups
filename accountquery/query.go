// Package accountquery provides the module-owned account investigation port.
package accountquery

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const Domain = "groups"
const Capability = Domain + ".account-query"
const SensitiveCapability = ""

var ErrInvalid = errors.New("invalid_account_query")
var ErrForbidden = errors.New("account_query_forbidden")
var ErrUnavailable = errors.New("account_query_unavailable")

type Principal struct{ Issuer, Subject string }

func (p Principal) Valid() bool {
	for _, s := range []string{p.Issuer, p.Subject} {
		if s == "" || len(s) > 1024 || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
			return false
		}
	}
	return base64.RawURLEncoding.EncodedLen(len(p.Issuer))+1+base64.RawURLEncoding.EncodedLen(len(p.Subject)) <= 2048
}
func (p Principal) Key() string {
	if !p.Valid() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(p.Issuer)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(p.Subject))
}
func Parse(key string) (Principal, error) {
	if len(key) > 2048 || strings.Count(key, ":") != 1 {
		return Principal{}, ErrInvalid
	}
	a, b, _ := strings.Cut(key, ":")
	i, err1 := base64.RawURLEncoding.Strict().DecodeString(a)
	s, err2 := base64.RawURLEncoding.Strict().DecodeString(b)
	p := Principal{string(i), string(s)}
	if err1 != nil || err2 != nil || !p.Valid() || p.Key() != key {
		return Principal{}, ErrInvalid
	}
	return p, nil
}

type Scope struct {
	School string `json:"school"`
	Group  string `json:"group"`
	Module string `json:"module"`
}

func (s Scope) Valid() bool {
	return clean(s.School, 128) && clean(s.Group, 128) && clean(s.Module, 128)
}
func (s Scope) Contains(t Scope) bool {
	return (s.School == "*" || s.School == t.School) && (s.Group == "*" || s.Group == t.Group) && (s.Module == "*" || s.Module == t.Module)
}
func clean(s string, n int) bool {
	return s != "" && len(s) <= n && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

type Query struct {
	Kind, Value string
	Scope       Scope
	Fields      []string
	Purpose     string
	RequestID   string
	Limit       int
	After       string
}

func (Query) String() string     { return "accountquery.Query{redacted}" }
func (q Query) GoString() string { return q.String() }

type Field struct {
	Name, Category, Value string
	Masked                bool
}
type Record struct {
	ID, Subject string
	Scope       Scope
	Source      string
	Current     bool
	ChangedAt   time.Time
	Fields      []Field
}

// Scopes must consult current trusted identity and grants for exactly capability.
// Empty grants mean forbidden. An adapter must never substitute another domain.
type Authorizer interface {
	Scopes(context.Context, Principal, string) ([]Scope, error)
}
type Audit struct {
	Actor                Principal
	RequestID, Purpose   string
	Scope                Scope
	Targets              []string
	Category, Capability string
	Result               string
	Count                int
	At                   time.Time
	Scopes               []Scope
}

// Record commits the disclosure audit before any sensitive result is returned.
type Auditor interface {
	Record(context.Context, Audit) error
}
type Selection struct {
	Query                   Query
	Scopes, SensitiveScopes []Scope
}
type Repository interface {
	ReadAccount(context.Context, Selection) ([]Record, error)
}
type Service struct {
	Repo  Repository
	Auth  Authorizer
	Audit Auditor
	Now   func() time.Time
}

func intersect(scopes []Scope, requested Scope) []Scope {
	out := []Scope{}
	for _, s := range scopes {
		if !s.Valid() {
			continue
		}
		a := []string{s.School, s.Group, s.Module}
		b := []string{requested.School, requested.Group, requested.Module}
		ok := true
		for i := range a {
			if a[i] == "*" {
				a[i] = b[i]
			} else if b[i] != "*" && a[i] != b[i] {
				ok = false
			}
		}
		if ok {
			out = append(out, Scope{a[0], a[1], a[2]})
		}
	}
	return out
}
func allowed(scopes []Scope, s Scope) bool {
	return slices.ContainsFunc(scopes, func(g Scope) bool { return g.Contains(s) })
}
func (s Service) Query(ctx context.Context, p Principal, q Query) ([]Record, error) {
	if !p.Valid() {
		return nil, ErrForbidden
	}
	if _, err := Parse(q.Value); err != nil {
		return nil, ErrInvalid
	}
	if q.Kind != "account" || !q.Scope.Valid() || (q.Scope.Module != "*" && q.Scope.Module != Domain) || q.Limit < 1 || q.Limit > 100 || len(q.After) > 256 || q.After != "" && !clean(q.After, 256) || !clean(q.Purpose, 512) || !clean(q.RequestID, 128) || len(q.Fields) > 5 {
		return nil, ErrInvalid
	}
	for _, f := range q.Fields {
		if !slices.Contains([]string{"business", "identity", "document", "material", "refund_payee"}, f) {
			return nil, ErrInvalid
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if s.Auth == nil || s.Repo == nil {
		return nil, ErrUnavailable
	}
	scopes, err := s.Auth.Scopes(ctx, p, Capability)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			return nil, ErrForbidden
		}
		return nil, ErrUnavailable
	}
	q.Scope.Module = Domain
	scopes = intersect(scopes, q.Scope)
	if len(scopes) == 0 {
		return nil, ErrForbidden
	}
	sensitive := []Scope{}
	if slices.Contains(q.Fields, "identity") && SensitiveCapability != "" {
		sensitive, err = s.Auth.Scopes(ctx, p, SensitiveCapability)
		if err != nil {
			if errors.Is(err, ErrForbidden) {
				if e := s.recordAudit(ctx, p, q, nil, nil, "denied"); e != nil {
					return nil, e
				}
				return nil, ErrForbidden
			}
			return nil, ErrUnavailable
		}
		sensitive = intersect(sensitive, q.Scope)
		if len(sensitive) == 0 {
			if e := s.recordAudit(ctx, p, q, nil, nil, "denied"); e != nil {
				return nil, e
			}
			return nil, ErrForbidden
		}
	}
	records, err := s.Repo.ReadAccount(ctx, Selection{q, scopes, sensitive})
	if err != nil {
		return nil, ErrUnavailable
	}
	if len(records) > q.Limit {
		return nil, ErrUnavailable
	}
	previous := q.After
	targets := []string{}
	for i := range records {
		r := &records[i]
		if !strings.HasPrefix(r.ID, Domain+":") || r.ID <= previous || r.Subject != q.Value || !r.Scope.Valid() || r.Scope.Module != Domain || !allowed(scopes, r.Scope) || r.ChangedAt.IsZero() {
			return nil, ErrUnavailable
		}
		previous = r.ID
		fields := []Field{}
		for _, f := range r.Fields {
			if f.Category == "business" {
				if len(q.Fields) == 0 || slices.Contains(q.Fields, "business") {
					fields = append(fields, f)
				}
				continue
			}
			if f.Category != "identity" {
				return nil, ErrUnavailable
			}
			if !slices.Contains(q.Fields, f.Category) || !allowed(sensitive, r.Scope) {
				f.Value = "***"
				f.Masked = true
			} else if !f.Masked {
				targets = append(targets, r.ID)
			}
			fields = append(fields, f)
		}
		r.Fields = fields
	}
	if len(sensitive) > 0 {
		if err = s.recordAudit(ctx, p, q, sensitive, targets, "success"); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func (s Service) recordAudit(ctx context.Context, p Principal, q Query, scopes []Scope, targets []string, result string) error {
	if s.Audit == nil {
		return ErrUnavailable
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	a := Audit{Actor: p, RequestID: q.RequestID, Purpose: q.Purpose, Scope: q.Scope, Targets: targets, Category: "identity", Capability: SensitiveCapability, Result: result, Count: len(targets), At: now, Scopes: scopes}
	if s.Audit.Record(ctx, a) != nil {
		return ErrUnavailable
	}
	return nil
}
