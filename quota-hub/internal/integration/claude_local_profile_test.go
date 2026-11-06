package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/engine"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

func TestClaudeLocalProfileDoesNotWaitForIdleOrChangeQuotaClocks(t *testing.T) {
	now := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	dir := t.TempDir()
	creds, account, observed := filepath.Join(dir, "credentials.json"), filepath.Join(dir, "account.json"), filepath.Join(dir, "headers.json")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(creds, fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"synthetic-access","expiresAt":%d,"subscriptionType":"max"}}`, now.Add(time.Hour).UnixMilli()))
	write(account, `{"oauthAccount":{"accountUuid":"pinned-account","emailAddress":"claude@example.test"}}`)
	headers := func() {
		write(observed, fmt.Sprintf(`{"schema_version":1,"source":"opencode-http-response-hook","observed_at":%q,"provider_id":"anthropic","status":200,"quota":[{"name":"anthropic-ratelimit-unified-5h-utilization","type":"number","value":0.2}]}`, now.Format(time.RFC3339Nano)))
	}
	headers()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("local metadata unexpectedly called remote API")
		w.WriteHeader(500)
	}))
	defer upstream.Close()
	pin := sha256.Sum256([]byte("pinned-account"))
	def := model.Definition{ID: "claude", Provider: "claude", Source: "opencode_observed", PublishAccountEmail: true, StaleAfter: time.Hour}
	spec := collectors.Spec{Definition: def, CredentialFile: creds, ClaudeAccountFile: account, ClaudeExpectedAccountSHA256: hex.EncodeToString(pin[:]), ObservedFile: observed, Interval: 5 * time.Minute, ObserveInterval: 30 * time.Second, ClaudeIdleAfter: 10 * time.Minute, Timeout: time.Second}
	c, err := collectors.New(spec, collectors.Options{Now: clock, Endpoints: map[string]string{"claude": upstream.URL}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "state"), []model.Definition{def})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	good := &model.Result{ObservedAt: now.Add(-time.Second), Quotas: []model.Quota{}, Metrics: []model.Metric{}}
	next := now.Add(30 * time.Second)
	passive := now.Add(-time.Second)
	remote := now.Add(time.Hour)
	hs := model.HybridState{LastPassiveAt: &passive, NextRemoteAttemptAt: &remote, RemoteFailures: 2, RemoteError: &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &remote}}
	if err := s.Commit(model.Update{ID: "claude", AttemptAt: now.Add(-time.Second), NextAttemptAt: next, Result: good, Hybrid: &hs}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetAt("claude", now)
	e, err := engine.New(s, []engine.Worker{{Spec: spec, Collector: c}}, engine.Options{Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := s.GetAt("claude", now)
	if sub.AccountEmail == nil || *sub.AccountEmail != "claude@example.test" || sub.Plan == nil || *sub.Plan != "max" {
		t.Fatal("startup did not fill profile from pinned local metadata")
	}
	if !sub.LastSuccessAt.Equal(*before.LastSuccessAt) || !sub.LastAttemptAt.Equal(*before.LastAttemptAt) || !sub.NextAttemptAt.Equal(*before.NextAttemptAt) || !sub.NextPollAt.Equal(*before.NextPollAt) {
		t.Fatal("profile changed quota clocks")
	}
	// Continuous passive traffic suppresses OAuth GET forever, but not metadata.
	for i := 0; i < 3; i++ {
		now = now.Add(30 * time.Second)
		headers()
		if _, err := e.Poll(context.Background(), "claude"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("profile bypassed remote cooldown")
	}
	if sub, _ = s.GetAt("claude", now); sub.Plan == nil || sub.AccountEmail == nil || sub.LastPollError == nil || sub.LastPollError.HTTPStatus != 429 || !sub.NextPollAt.Equal(remote) {
		t.Fatal("passive scans lost profile or cooldown")
	}
	// A local metadata failure does not fail the successful header collection.
	write(account, `{"oauthAccount":`)
	now = now.Add(30 * time.Second)
	headers()
	if _, err := e.Poll(context.Background(), "claude"); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetAt("claude", now); sub.AccountEmail != nil || sub.Plan != nil || sub.Status != "ok" {
		t.Fatal("metadata failure leaked profile or failed usage")
	}
	// Successful confirmation of absence clears old metadata for the same account.
	write(account, `{"oauthAccount":{"accountUuid":"pinned-account"}}`)
	write(creds, fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"synthetic-access","expiresAt":%d}}`, now.Add(time.Hour).UnixMilli()))
	now = now.Add(30 * time.Second)
	headers()
	if _, err := e.Poll(context.Background(), "claude"); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetAt("claude", now); sub.AccountEmail != nil || sub.Plan != nil || sub.Status != "ok" {
		t.Fatal("authoritative absence not respected")
	}
	// Different account stays blocked, even while usage headers remain healthy.
	write(account, `{"oauthAccount":{"accountUuid":"other-account","emailAddress":"other@example.test"}}`)
	now = now.Add(30 * time.Second)
	headers()
	if _, err := e.Poll(context.Background(), "claude"); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetAt("claude", now); sub.AccountEmail != nil || sub.Plan != nil || sub.Status != "ok" || calls.Load() != 0 {
		t.Fatal("cross-account metadata escaped pin or changed usage")
	}
}
