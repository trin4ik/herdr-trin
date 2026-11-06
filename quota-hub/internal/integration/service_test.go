// These tests use only synthetic credentials, local fake upstreams and temporary state.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/engine"
	"quota-hub/internal/httpapi"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

func TestMixedSourcesThroughStorageAndHTTPWithRestart(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var calls atomic.Int32
	var throttleOllama atomic.Bool
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/credential":
			fmt.Fprint(w, `{"data":[{"id":"ollama-account","integrationID":"ollama-cloud","active":true,"value":{"type":"key","key":"FAKE-OLLAMA-KEY"}},{"id":"kimi-account","integrationID":"kimi-code-plan-cn","active":true,"value":{"type":"key","key":"FAKE-KIMI-KEY"}}]}`)
		case "/codex":
			if r.Header.Get("Authorization") != "Bearer FAKE-CODEX-TOKEN" || r.Header.Get("ChatGPT-Account-Id") != "FAKE-ACCOUNT" {
				t.Error("wrong synthetic Codex auth")
			}
			fmt.Fprint(w, `{"email":"PRIVATE_ACCOUNT_MARKER","plan_type":"prolite","rate_limit":{"primary_window":{"used_percent":32,"limit_window_seconds":604800,"reset_at":1794343662},"secondary_window":null}}`)
		case "/ollama":
			if throttleOllama.Load() {
				w.Header().Set("Retry-After", "86400")
				w.WriteHeader(429)
				return
			}
			fmt.Fprint(w, `{"included":{"session":{"remaining_percent":95,"resets_at":"2026-11-04T15:00:00Z"},"weekly":{"remaining_percent":99.46}},"purchased":{"balance_usd":0}}`)
		case "/kimi":
			fmt.Fprint(w, `{"usages":{"limit_5h":{"used_ratio":0},"limit_month_total":{"used_ratio":0.1125},"limit_month_code":{"used_ratio":0.1125}},"booster_wallet":{"userId":"PRIVATE_ACCOUNT_MARKER"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	tmp := t.TempDir()
	auth := filepath.Join(tmp, "codex.json")
	if err := os.WriteFile(auth, []byte(`{"tokens":{"access_token":"FAKE-CODEX-TOKEN","account_id":"FAKE-ACCOUNT","refresh_token":"NEVER_READ_OR_WRITE"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := filepath.Join(tmp, "claude.latest.json")
	observation := `{"schema_version":1,"source":"opencode-http-response-hook","provider_id":"anthropic","observed_at":"2026-11-04T11:59:00Z","status":200,"quota":[{"name":"anthropic-ratelimit-unified-5h-utilization","type":"number","value":0.38},{"name":"anthropic-ratelimit-unified-7d-utilization","type":"number","value":0.5}]}`
	if err := os.WriteFile(observed, []byte(observation), 0o600); err != nil {
		t.Fatal(err)
	}
	var definitions []model.Definition
	var workers []engine.Worker
	for _, provider := range []string{"codex", "ollama", "kimi", "claude"} {
		def := model.Definition{ID: provider, Provider: provider, Product: provider, StaleAfter: 30 * time.Minute}
		spec := collectors.Spec{Definition: def, Interval: 15 * time.Minute, Timeout: time.Second, Scope: provider,
			OpenCodeURL: fake.URL, RateLimitPause: 15 * time.Minute}
		switch provider {
		case "codex":
			spec.Source, spec.CredentialFile = "native_codex_oauth", auth
		case "ollama":
			spec.Source, spec.IntegrationID = "opencode_key", "ollama-cloud"
		case "kimi":
			spec.Source, spec.IntegrationID = "opencode_key", "kimi-code-plan-cn"
		case "claude":
			spec.Source, spec.ObservedFile = "opencode_observed", observed
			spec.Interval = 30 * time.Second
		}
		definitions = append(definitions, spec.Definition)
		collector, err := collectors.New(spec, collectors.Options{Now: clock, Endpoints: map[string]string{
			"codex": fake.URL + "/codex", "ollama": fake.URL + "/ollama", "kimi": fake.URL + "/kimi",
		}})
		if err != nil {
			t.Fatal(err)
		}
		workers = append(workers, engine.Worker{Spec: spec, Collector: collector})
	}
	data := filepath.Join(tmp, "store")
	s, err := store.Open(data, definitions)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	e, err := engine.New(s, workers, engine.Options{Now: clock, BackoffBase: time.Minute, BackoffMax: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range workers {
		if tried, err := e.Poll(context.Background(), worker.Spec.ID); !tried || err != nil {
			t.Fatalf("initial collection %s: tried=%v err=%v", worker.Spec.ID, tried, err)
		}
	}
	snapshot := s.Snapshot(now)
	for i, count := range []int{1, 2, 3, 2} {
		if len(snapshot.Subscriptions[i].Quotas) != count || snapshot.Subscriptions[i].Status != "ok" {
			t.Fatalf("wrong normalized result for %s", snapshot.Subscriptions[i].ID)
		}
	}
	const token = "a-test-only-bearer-token-of-sufficient-length"
	handler := httpapi.New(s, token, clock)
	before := calls.Load()
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/quotas", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != 200 {
			t.Fatal("cached HTTP request failed")
		}
		for _, marker := range []string{"PRIVATE_ACCOUNT_MARKER", "FAKE-CODEX-TOKEN", "FAKE-OLLAMA-KEY", "FAKE-KIMI-KEY", "NEVER_READ_OR_WRITE"} {
			if strings.Contains(recorder.Body.String(), marker) {
				t.Fatal("private information leaked in HTTP")
			}
		}
		var decoded model.Snapshot
		if json.Unmarshal(recorder.Body.Bytes(), &decoded) != nil {
			t.Fatal("HTTP snapshot is not valid JSON")
		}
	}
	if before != calls.Load() {
		t.Fatal("HTTP clients triggered upstream traffic")
	}
	if tried, _ := e.Poll(context.Background(), "ollama"); tried {
		t.Fatal("manual collection bypassed interval")
	}
	now = now.Add(16 * time.Minute)
	throttleOllama.Store(true)
	if tried, err := e.Poll(context.Background(), "ollama"); !tried || err != nil {
		t.Fatal("failed to record throttled attempt")
	}
	sub, _ := s.GetAt("ollama", now)
	if !sub.Stale || sub.Error == nil || sub.Error.HTTPStatus != 429 || len(sub.Quotas) != 2 || sub.NextAttemptAt.Before(now.Add(24*time.Hour)) {
		t.Fatal("429 lost last-good or server cooldown")
	}
	if _, err := e.Poll(context.Background(), "claude"); err != nil {
		t.Fatal(err)
	}
	lines, err := os.ReadFile(filepath.Join(data, "history", "claude.jsonl"))
	if err != nil || len(strings.Split(strings.TrimSpace(string(lines)), "\n")) != 1 {
		t.Fatal("unchanged passive observation duplicated history")
	}
	s.Close()
	s, err = store.Open(data, definitions)
	if err != nil {
		t.Fatal(err)
	}
	e, err = engine.New(s, workers, engine.Options{Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	if tried, err := e.Poll(context.Background(), "ollama"); tried || err != nil || before != calls.Load() {
		t.Fatal("restart erased persisted cooldown")
	}
}
