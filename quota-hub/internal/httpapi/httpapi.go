// Package httpapi serves the cached public snapshot. It never contacts providers.
package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quota-hub/internal/store"
)

type api struct {
	st    *store.Store
	token [sha256.Size]byte
	has   bool
	open  bool
	now   func() time.Time
}

// New returns the handler. With an empty token every protected endpoint answers
// 401, so a misconfigured caller can never expose the snapshot unauthenticated.
// now defaults to time.Now.
func New(s *store.Store, token string, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	return &api{st: s, token: sha256.Sum256([]byte(token)), has: token != "", now: now}
}

// NewUnauthenticated is an explicit operator opt-out. The listener and network
// proxy must enforce the chosen private-network boundary. New stays fail-closed.
func NewUnauthenticated(s *store.Store, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	return &api{st: s, open: true, now: now}
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")

	var handler func(http.ResponseWriter)
	protected := false
	switch r.URL.Path {
	case "/", "/v1/quotas":
		handler, protected = a.quotas, true
	case "/healthz":
		handler = func(w http.ResponseWriter) { write(w, http.StatusOK, []byte(`{"status":"ok"}`)) }
	case "/readyz":
		handler = a.ready
	default:
		write(w, http.StatusNotFound, []byte(`{"error":"not_found"}`))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		write(w, http.StatusMethodNotAllowed, []byte(`{"error":"method_not_allowed"}`))
		return
	}
	if protected && !a.open && !a.authorized(r) {
		h.Set("WWW-Authenticate", `Bearer realm="quota-hub"`)
		write(w, http.StatusUnauthorized, []byte(`{"error":"unauthorized"}`))
		return
	}
	handler(w)
}

func (a *api) authorized(r *http.Request) bool {
	if !a.has {
		return false
	}
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(v[len(prefix):])))
	return subtle.ConstantTimeCompare(got[:], a.token[:]) == 1
}

func (a *api) quotas(w http.ResponseWriter) {
	if !a.st.Ready() {
		write(w, http.StatusServiceUnavailable, []byte(`{"error":"not_ready"}`))
		return
	}
	body, err := json.Marshal(a.st.Snapshot(a.now()))
	if err != nil {
		write(w, http.StatusInternalServerError, []byte(`{"error":"internal"}`))
		return
	}
	write(w, http.StatusOK, body)
}

func (a *api) ready(w http.ResponseWriter) {
	if a.st.Ready() {
		write(w, http.StatusOK, []byte(`{"status":"ready"}`))
		return
	}
	write(w, http.StatusServiceUnavailable, []byte(`{"status":"not_ready"}`))
}

// write sends body; net/http drops it for HEAD while keeping Content-Length.
func write(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}
