package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

var t0 = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

const (
	token  = "s3cret-bearer"
	rawSec = "RAW-PROVIDER-TOKEN"
)

func setup(t *testing.T, now *time.Time) (*httptest.Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir, []model.Definition{
		{ID: "claude", Provider: "claude", Product: "code", Source: "claude_code", StaleAfter: 30 * time.Minute},
		{ID: "kimi", Provider: "kimi", Product: "code", Source: "opencode", StaleAfter: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := httptest.NewServer(New(s, token, func() time.Time { return *now }))
	t.Cleanup(srv.Close)
	return srv, s, dir
}

func TestExplicitNoAuthServesCachedJSON(t *testing.T) {
	now := t0
	_, s, _ := setup(t, &now)
	commitOK(t, s, "claude", now)
	srv := httptest.NewServer(NewUnauthenticated(s, func() time.Time { return now }))
	defer srv.Close()
	for _, path := range []string{"/", "/v1/quotas"} {
		resp, body := do(t, http.MethodGet, srv.URL+path, "")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("WWW-Authenticate") != "" || strings.Contains(body, rawSec) {
			t.Fatal("explicit no-auth endpoint failed or leaked raw state")
		}
		var snapshot model.Snapshot
		if json.Unmarshal([]byte(body), &snapshot) != nil || len(snapshot.Subscriptions) != 2 {
			t.Fatal("no-auth did not return the cached public snapshot")
		}
	}
}

func do(t *testing.T, method, url, auth string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func commitOK(t *testing.T, s *store.Store, id string, at time.Time) {
	t.Helper()
	used := 12.5
	err := s.Commit(model.Update{ID: id, AttemptAt: at, NextAttemptAt: at.Add(10 * time.Minute), Result: &model.Result{
		ObservedAt: at,
		Quotas:     []model.Quota{{ID: "5h", Metric: "percent", Unit: "percent", UsedPercent: &used, Window: model.Window{Kind: "rolling"}}},
		Raw:        json.RawMessage(`{"access_token":"` + rawSec + `"}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuth(t *testing.T) {
	now := t0
	srv, _, _ := setup(t, &now)
	for _, p := range []string{"/", "/v1/quotas"} {
		for _, auth := range []string{"", "Bearer wrong", "Bearer ", "Basic " + token, token, "Bearer  "} {
			resp, body := do(t, "GET", srv.URL+p, auth)
			if resp.StatusCode != 401 || strings.Contains(body, "subscriptions") {
				t.Fatalf("%s %q: %d %s", p, auth, resp.StatusCode, body)
			}
		}
		resp, body := do(t, "GET", srv.URL+p, "Bearer "+token)
		if resp.StatusCode != 200 || !strings.Contains(body, `"schema_version":1`) {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
}

func TestEmptyTokenNeverExposes(t *testing.T) {
	now := t0
	_, s, _ := setup(t, &now)
	h := New(s, "", nil)
	for _, auth := range []string{"", "Bearer ", "Bearer x"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/quotas", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("%q: %d", auth, rec.Code)
		}
	}
}

func TestHeadersMethodsRoutes(t *testing.T) {
	now := t0
	srv, _, _ := setup(t, &now)
	resp, _ := do(t, "GET", srv.URL+"/v1/quotas", "Bearer "+token)
	h := resp.Header
	if h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-store" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("%v", h)
	}
	resp, body := do(t, "HEAD", srv.URL+"/v1/quotas", "Bearer "+token)
	if resp.StatusCode != 200 || body != "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		resp, _ := do(t, m, srv.URL+"/v1/quotas", "Bearer "+token)
		if resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s: %d", m, resp.StatusCode)
		}
		if resp, _ := do(t, m, srv.URL+"/healthz", ""); resp.StatusCode != 405 {
			t.Fatalf("%s healthz: %d", m, resp.StatusCode)
		}
	}
	for _, p := range []string{"/nope", "/v1/quotas/", "/v1", "/admin", "/..%2f"} {
		resp, _ := do(t, "GET", srv.URL+p, "Bearer "+token)
		if resp.StatusCode != 404 || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestHealthAndReady(t *testing.T) {
	now := t0
	srv, s, _ := setup(t, &now)
	for _, p := range []string{"/healthz", "/readyz"} {
		if resp, body := do(t, "GET", srv.URL+p, ""); resp.StatusCode != 200 {
			t.Fatalf("%s %d %s", p, resp.StatusCode, body)
		}
	}
	s.Close()
	if resp, _ := do(t, "GET", srv.URL+"/readyz", ""); resp.StatusCode != 503 {
		t.Fatal(resp.StatusCode)
	}
	if resp, _ := do(t, "GET", srv.URL+"/healthz", ""); resp.StatusCode != 200 {
		t.Fatal("liveness must not depend on store")
	}
	if resp, _ := do(t, "GET", srv.URL+"/v1/quotas", "Bearer "+token); resp.StatusCode != 503 {
		t.Fatal(resp.StatusCode)
	}
	h := New(nil, token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatal(rec.Code)
	}
}

func snapshot(t *testing.T, url string) model.Snapshot {
	t.Helper()
	_, body := do(t, "GET", url+"/v1/quotas", "Bearer "+token)
	var snap model.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatal(err, body)
	}
	return snap
}

func TestCachedOnlyAndUnknowns(t *testing.T) {
	now := t0
	srv, s, dir := setup(t, &now)
	commitOK(t, s, "claude", t0)
	count := func() int {
		b, _ := os.ReadFile(filepath.Join(dir, "attempts", "claude.jsonl"))
		return strings.Count(string(b), "\n")
	}
	before := count()
	for i := 0; i < 10; i++ {
		snapshot(t, srv.URL)
	}
	if count() != before {
		t.Fatal("GET changed persisted attempts")
	}
	snap := snapshot(t, srv.URL)
	if len(snap.Subscriptions) != 2 || snap.Subscriptions[0].ID != "claude" {
		t.Fatalf("%+v", snap)
	}
	kimi := snap.Subscriptions[1]
	if kimi.Status != "unknown" || !kimi.Stale || kimi.LastSuccessAt != nil || kimi.Quotas == nil || len(kimi.Quotas) != 0 {
		t.Fatalf("%+v", kimi)
	}
	_, raw := do(t, "GET", srv.URL+"/", "Bearer "+token)
	if strings.Contains(raw, `null,"quotas":null`) || strings.Contains(raw, `"quotas":null`) || strings.Contains(raw, `"metrics":null`) {
		t.Fatalf("null arrays: %s", raw)
	}
	if !strings.Contains(raw, `"quotas":[]`) {
		t.Fatalf("missing []: %s", raw)
	}
}

func TestStaleAgeAndLastGoodAfterFailure(t *testing.T) {
	now := t0
	srv, s, _ := setup(t, &now)
	commitOK(t, s, "claude", t0)
	now = t0.Add(10 * time.Minute)
	c := snapshot(t, srv.URL).Subscriptions[0]
	if c.Stale || c.Status != "ok" {
		t.Fatalf("%+v", c)
	}
	now = t0.Add(31 * time.Minute)
	c = snapshot(t, srv.URL).Subscriptions[0]
	if !c.Stale || c.Status != "stale" || len(c.Quotas) != 1 || *c.Quotas[0].UsedPercent != 12.5 {
		t.Fatalf("%+v", c)
	}
	if !c.LastSuccessAt.Equal(t0) {
		t.Fatalf("last success must be source time: %v", c.LastSuccessAt)
	}
	now = t0.Add(time.Minute)
	s.Commit(model.Update{ID: "claude", AttemptAt: now, NextAttemptAt: now.Add(time.Hour),
		Failure: &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429}})
	c = snapshot(t, srv.URL).Subscriptions[0]
	if !c.Stale || c.Error == nil || c.Error.Code != "rate_limited" || len(c.Quotas) != 1 || *c.Quotas[0].UsedPercent != 12.5 {
		t.Fatalf("last-good lost: %+v", c)
	}
}

func TestNoSecretsInHTTP(t *testing.T) {
	now := t0
	srv, s, dir := setup(t, &now)
	commitOK(t, s, "claude", t0)
	s.Commit(model.Update{ID: "claude", AttemptAt: t0.Add(time.Second), NextAttemptAt: t0.Add(time.Hour),
		Failure: &model.ErrorInfo{Code: "bad token " + rawSec + " " + dir}})
	for _, p := range []string{"/", "/v1/quotas", "/healthz", "/readyz", "/nope"} {
		_, body := do(t, "GET", srv.URL+p, "Bearer "+token)
		if strings.Contains(body, rawSec) || strings.Contains(body, dir) || strings.Contains(body, token) {
			t.Fatalf("%s leaked: %s", p, body)
		}
	}
	resp, body := do(t, "GET", srv.URL+"/v1/quotas", "Bearer wrong")
	if strings.Contains(body, token) || strings.Contains(resp.Header.Get("WWW-Authenticate"), token) {
		t.Fatal("token echoed")
	}
}
