// Package httpapi accepts principals only from the host authentication middleware.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
	"github.com/StuHelper/stuhelper-platform/admin"
	"github.com/getkin/kin-openapi/openapi3"
)

type Identity func(*http.Request) (account.Principal, error)

func Handler(s *g.Service, identity Identity) http.Handler {
	mux := http.NewServeMux()
	wrap := func(fn func(http.ResponseWriter, *http.Request, account.Principal)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if identity == nil {
				write(w, nil, g.ErrUnauthenticated)
				return
			}
			p, e := identity(r)
			if e != nil || !p.Valid() {
				write(w, nil, g.ErrUnauthenticated)
				return
			}
			fn(w, r, p)
		}
	}
	mux.HandleFunc("GET /api/v1/groups/me", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		for name := range r.URL.Query() {
			if name != "cursor" && name != "limit" {
				write(w, nil, g.ErrInvalid)
				return
			}
		}
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.MyGroups(r.Context(), p, cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		v, e := s.Configuration(r.Context(), p, r.PathValue("group"))
		write(w, v, e)
	}))
	mux.HandleFunc("PUT /api/v1/groups/{group}", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		var c g.GroupConfigInput
		if e := decode(w, r, "GroupConfigInput", &c); e != nil {
			write(w, nil, e)
			return
		}
		if c.ID != r.PathValue("group") {
			write(w, nil, g.ErrInvalid)
			return
		}
		v, e := s.Configure(r.Context(), p, r.Header.Get("Idempotency-Key"), c)
		write(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/groups/{group}/commands", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		var c g.Command
		if e := decode(w, r, "Command", &c); e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Command(r.Context(), p, r.PathValue("group"), r.Header.Get("Idempotency-Key"), c)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Groups(r.Context(), p, cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/progress", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		v, e := s.Progress(r.Context(), p, r.PathValue("group"))
		write(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/groups/{group}/batch", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		var in g.Bulk
		if e := decode(w, r, "Bulk", &in); e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Batch(r.Context(), p, r.PathValue("group"), r.Header.Get("Idempotency-Key"), in)
		write(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/groups/blacklists", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		var in g.BlacklistInput
		if e := decode(w, r, "BlacklistInput", &in); e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Blacklists(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/members", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Members(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/actions", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Actions(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/reports", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Reports(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/hits", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Hits(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/messages", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Messages(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/subscriptions", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Subscriptions(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/blacklist", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Blacklist(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/warnings", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Warnings(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/watch", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Watch(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/deliveries", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		cursor, limit, e := pagination(r)
		if e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Deliveries(r.Context(), p, r.PathValue("group"), cursor, limit)
		write(w, v, e)
	}))
	return mux
}
func pagination(r *http.Request) (string, int, error) {
	q := r.URL.Query()
	if len(q["limit"]) > 1 || len(q["cursor"]) > 1 || len(q.Get("cursor")) > 4096 {
		return "", 0, g.ErrInvalid
	}
	limit := 50
	if raw, exists := q["limit"]; exists {
		n, e := strconv.Atoi(raw[0])
		if e != nil || n < 1 || n > 100 {
			return "", 0, g.ErrInvalid
		}
		limit = n
	}
	return q.Get("cursor"), limit, nil
}

var contract = sync.OnceValues(func() (*openapi3.T, error) { return openapi3.NewLoader().LoadFromData(g.OpenAPI()) })

func decode(w http.ResponseWriter, r *http.Request, schema string, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 32768)
	data, e := io.ReadAll(r.Body)
	if e != nil {
		return g.ErrInvalid
	}
	var value any
	if e = json.Unmarshal(data, &value); e != nil {
		return g.ErrInvalid
	}
	doc, e := contract()
	if e != nil {
		return g.ErrUnavailable
	}
	if e = doc.Components.Schemas[schema].Value.VisitJSON(value); e != nil {
		return g.ErrUnprocessable
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return g.ErrUnprocessable
	}
	return nil
}
func write(w http.ResponseWriter, v any, e error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if e != nil {
		status, code := 503, g.ErrUnavailable.Error()
		for _, pair := range []struct {
			err    error
			status int
			code   string
		}{
			{g.ErrInvalid, 400, g.ErrInvalid.Error()}, {g.ErrUnauthenticated, 401, g.ErrUnauthenticated.Error()}, {admin.ErrUnauthenticated, 401, g.ErrUnauthenticated.Error()},
			{g.ErrForbidden, 403, g.ErrForbidden.Error()}, {admin.ErrForbidden, 403, g.ErrForbidden.Error()}, {g.ErrNotFound, 404, g.ErrNotFound.Error()},
			{g.ErrConflict, 409, g.ErrConflict.Error()}, {g.ErrCooldown, 409, g.ErrCooldown.Error()}, {g.ErrUnprocessable, 422, g.ErrUnprocessable.Error()}, {g.ErrDisabled, 422, g.ErrDisabled.Error()},
		} {
			if errors.Is(e, pair.err) {
				status = pair.status
				code = pair.code
				break
			}
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
