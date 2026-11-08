// Package httpapi accepts principals only from the host authentication middleware.
package httpapi

import (
	"encoding/json"
	"errors"
	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-platform/account"
	"github.com/StuHelper/stuhelper-platform/admin"
	"io"
	"net/http"
	"strconv"
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
	mux.HandleFunc("GET /api/v1/groups/{group}", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		v, e := s.Configuration(r.Context(), p, r.PathValue("group"))
		write(w, v, e)
	}))
	mux.HandleFunc("PUT /api/v1/groups/{group}", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		var c g.Config
		if e := decode(w, r, &c); e != nil {
			write(w, nil, e)
			return
		}
		if c.ID != r.PathValue("group") {
			write(w, nil, g.ErrInvalid)
			return
		}
		v, e := s.Configure(r.Context(), p, r.Header.Get("Idempotency-Key"), c)
		v.PasswordHash = ""
		write(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/groups/{group}/commands", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		var c g.Command
		if e := decode(w, r, &c); e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Command(r.Context(), p, r.PathValue("group"), r.Header.Get("Idempotency-Key"), c)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/{collection}", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil {
				write(w, nil, g.ErrInvalid)
				return
			}
			limit = n
		}
		v, e := s.List(r.Context(), p, r.PathValue("group"), r.PathValue("collection"), r.URL.Query().Get("cursor"), limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil {
				write(w, nil, g.ErrInvalid)
				return
			}
			limit = n
		}
		v, e := s.Groups(r.Context(), p, r.URL.Query().Get("cursor"), limit)
		write(w, v, e)
	}))
	mux.HandleFunc("GET /api/v1/groups/{group}/progress", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		v, e := s.Progress(r.Context(), p, r.PathValue("group"))
		write(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/groups/{group}/batch", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		var in g.Bulk
		if e := decode(w, r, &in); e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Batch(r.Context(), p, r.PathValue("group"), r.Header.Get("Idempotency-Key"), in)
		write(w, v, e)
	}))
	mux.HandleFunc("POST /api/v1/groups/blacklists", wrap(func(w http.ResponseWriter, r *http.Request, p account.Principal) {
		var in g.BlacklistInput
		if e := decode(w, r, &in); e != nil {
			write(w, nil, e)
			return
		}
		v, e := s.Blacklists(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
		write(w, v, e)
	}))
	return mux
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 32768)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return g.ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF {
		return g.ErrInvalid
	}
	return nil
}
func write(w http.ResponseWriter, v any, e error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if e != nil {
		status, code := 503, "unavailable"
		for _, pair := range []struct {
			err    error
			status int
			code   string
		}{{g.ErrInvalid, 400, "invalid_request"}, {g.ErrUnauthenticated, 401, "unauthenticated"}, {g.ErrForbidden, 403, "forbidden"}, {admin.ErrForbidden, 403, "forbidden"}, {g.ErrNotFound, 404, "not_found"}, {g.ErrConflict, 409, "conflict"}, {g.ErrDisabled, 409, "disabled"}, {g.ErrCooldown, 429, "cooldown"}} {
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
