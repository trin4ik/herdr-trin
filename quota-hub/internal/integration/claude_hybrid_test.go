package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

func TestClaudeHeadersNativeOAuthAndCooldownComposeAcrossRestart(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	start := now
	clock := func() time.Time { return now }
	var calls atomic.Int32
	var limited atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer NATIVE-ACCESS-ONLY" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Error("wrong native OAuth quota request")
		}
		if limited.Load() {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"five_hour":{"utilization":25},"seven_day":{"utilization":55}}`)
	}))
	defer api.Close()
	dir := t.TempDir()
	creds := filepath.Join(dir, ".credentials.json")
	credentialBytes := []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"NATIVE-ACCESS-ONLY","refreshToken":"NEVER-USE-REFRESH","expiresAt":%d}}`, now.Add(24*time.Hour).UnixMilli()))
	if err := os.WriteFile(creds, credentialBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	account := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(account, []byte(`{"oauthAccount":{"accountUuid":"same-account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256([]byte("same-account"))
	observed := filepath.Join(dir, "claude.latest.json")
	writeHeader := func(at time.Time) {
		t.Helper()
		body := fmt.Sprintf(`{"schema_version":1,"source":"opencode-http-response-hook","observed_at":%q,"provider_id":"anthropic","status":200,"quota":[{"name":"anthropic-ratelimit-unified-5h-utilization","type":"number","value":0.2}]}`, at.Format(time.RFC3339Nano))
		if err := os.WriteFile(observed, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeHeader(now)
	def := model.Definition{ID: "claude", Provider: "claude", Product: "claude_subscription", Source: "opencode_observed", StaleAfter: 30 * time.Minute}
	spec := collectors.Spec{Definition: def, Interval: 5 * time.Minute, ObserveInterval: 30 * time.Second,
		ClaudeNative: true, Timeout: time.Second, Scope: "claude", RateLimitPause: 15 * time.Minute,
		ObservedFile: observed, CredentialFile: creds, ClaudeAccountFile: account, ClaudeExpectedAccountSHA256: hex.EncodeToString(pin[:])}
	collector, err := collectors.New(spec, collectors.Options{Now: clock, Endpoints: map[string]string{"claude": api.URL}})
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "store")
	s, err := store.Open(data, []model.Definition{def})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	makeEngine := func() *engine.Engine {
		t.Helper()
		e, err := engine.New(s, []engine.Worker{{Spec: spec, Collector: collector}}, engine.Options{Now: clock})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e := makeEngine()
	poll := func() model.Subscription {
		t.Helper()
		if _, err := e.Poll(context.Background(), "claude"); err != nil {
			t.Fatal(err)
		}
		sub, _ := s.GetAt("claude", now)
		return sub
	}
	sub := poll()
	if calls.Load() != 1 || sub.Source != "claude_code_oauth" || sub.LastPassiveAt == nil || !sub.LastPassiveAt.Equal(start) {
		t.Fatal("the native GET must follow the subscription interval and the bridge clock must stay separate")
	}
	now = start.Add(10 * time.Minute)
	sub = poll()
	if calls.Load() != 2 || sub.Source != "claude_code_oauth" || !sub.LastPassiveAt.Equal(start) || !sub.LastSuccessAt.Equal(now) {
		t.Fatal("remote polling did not preserve separate bridge/API clocks")
	}
	now = start.Add(10*time.Minute + 30*time.Second)
	sub = poll()
	if calls.Load() != 2 || !sub.LastSuccessAt.Equal(start.Add(10*time.Minute)) {
		t.Fatal("unchanged old bridge freshened or replaced the OAuth snapshot")
	}
	now = start.Add(15 * time.Minute)
	if sub = poll(); calls.Load() != 3 || !sub.LastPassiveAt.Equal(start) {
		t.Fatal("API success incorrectly made the bridge look active")
	}
	limited.Store(true)
	now = start.Add(20 * time.Minute)
	sub = poll()
	deadline := start.Add(80 * time.Minute)
	if calls.Load() != 4 || sub.Error == nil || sub.Error.HTTPStatus != 429 || sub.NextPollAt == nil || sub.NextPollAt.Before(deadline) {
		t.Fatal("remote throttle deadline not exposed/preserved")
	}
	now = start.Add(20*time.Minute + 30*time.Second)
	writeHeader(now)
	sub = poll()
	if calls.Load() != 4 || sub.Stale || sub.Error != nil || sub.Source != "opencode_observed" || sub.LastPollError == nil || sub.LastPollError.HTTPStatus != 429 || sub.NextPollAt.Before(deadline) {
		t.Fatal("remote cooldown blocked passive headers, or headers erased the cooldown")
	}
	s.Close()
	s, err = store.Open(data, []model.Definition{def})
	if err != nil {
		t.Fatal(err)
	}
	e = makeEngine()
	now = start.Add(21 * time.Minute)
	writeHeader(now)
	if sub = poll(); sub.Stale || calls.Load() != 4 || sub.NextPollAt.Before(deadline) {
		t.Fatal("restart lost the independent remote pause or blocked the passive reader")
	}
	now = start.Add(31 * time.Minute)
	if sub = poll(); calls.Load() != 4 || !sub.LastSuccessAt.Equal(start.Add(21*time.Minute)) {
		t.Fatal("scan bypassed a persisted 429 deadline")
	}
	limited.Store(false)
	now = start.Add(81 * time.Minute)
	if sub = poll(); calls.Load() != 5 || sub.LastPollError != nil || sub.Source != "claude_code_oauth" {
		t.Fatal("polling did not recover after the real server deadline")
	}
	unchanged, err := os.ReadFile(creds)
	if err != nil || string(unchanged) != string(credentialBytes) {
		t.Fatal("hybrid modified native credentials")
	}
	public, _ := json.Marshal(s.Snapshot(now))
	for _, private := range []string{"NATIVE-ACCESS-ONLY", "NEVER-USE-REFRESH", "same-account", creds, account} {
		if strings.Contains(string(public), private) {
			t.Fatal("private credentials/account data leaked in the public hybrid view")
		}
	}
}
