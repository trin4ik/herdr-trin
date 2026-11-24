package integration_test

import (
	"context"
	"encoding/json"
	"errors"
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

func TestGoogleAntigravityProfileAndExpiryFlow(t *testing.T) {
	for _, publish := range []bool{false, true} {
		t.Run(fmt.Sprintf("publish=%v", publish), func(t *testing.T) {
			var account, tokenGen, mode, calls, profileCalls atomic.Int32
			account.Store(1)
			tokenGen.Store(1)
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch r.URL.Path {
				case "/api/credential":
					n := account.Load()
					gen := tokenGen.Load()
					expires := int64(1900000000000)
					if mode.Load() == 2 {
						expires = 1700000000000 // Expired token: must fail locally with auth_required.
					}
					fmt.Fprintf(w, `{"data":[{"id":"cred-google","integrationID":"google","active":true,"value":{"type":"oauth","methodID":"antigravity","access":"syn-token-%d-%d","refresh":"rt-unused","expires":%d,"metadata":{"antigravityAccountId":"acct-%d","email":"user%d@example.test"}}}]}`, n, gen, expires, n, n)
				case "/v1internal:retrieveUserQuotaSummary":
					if mode.Load() == 2 {
						t.Error("expired token must not hit upstream quota summary")
					}
					fmt.Fprint(w, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","resetTime":"2026-11-30T21:00:00Z","remainingFraction":0.8},{"bucketId":"gemini-5h","window":"5h","resetTime":"2026-11-24T02:00:00Z","remainingFraction":0.5}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","resetTime":"2026-11-24T05:00:00Z","remainingFraction":1},{"bucketId":"3p-weekly","window":"weekly","resetTime":"2026-12-01T00:00:00Z","remainingFraction":1}]}]}`)
				case "/v1internal:loadCodeAssist":
					profileCalls.Add(1)
					fmt.Fprint(w, `{"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"},"cloudaicompanionProject":"synthetic"}`)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer fake.Close()

			dir := t.TempDir()
			reg := filepath.Join(dir, "service.json")
			if err := os.WriteFile(reg, []byte(fmt.Sprintf(`{"url":%q}`, fake.URL)), 0600); err != nil {
				t.Fatal(err)
			}
			id := "google-primary"
			provider := "google"
			spec := collectors.Spec{
				Definition: model.Definition{
					ID: id, Provider: collectors.ProviderGoogle, Source: collectors.SourceOpenCodeAntigravity,
					Product: "google_antigravity", PublishAccountEmail: publish, StaleAfter: time.Hour,
					OpenCodeProvider: &provider, OpenCodeProviders: []string{provider},
				},
				IntegrationID: collectors.GoogleAntigravityIntegration, OpenCodeServiceFile: reg,
				Interval: time.Minute, Timeout: time.Second, AuthRetry: time.Minute,
			}
			now := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			c, err := collectors.New(spec, collectors.Options{
				Now: clock,
				Endpoints: map[string]string{
					"google_quota":   fake.URL + "/v1internal:retrieveUserQuotaSummary",
					"google_profile": fake.URL + "/v1internal:loadCodeAssist",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			open := func() (*store.Store, *engine.Engine) {
				t.Helper()
				s, err := store.Open(filepath.Join(dir, "store"), []model.Definition{spec.Definition})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				e, err := engine.New(s, []engine.Worker{{Spec: spec, Collector: c}}, engine.Options{Now: clock})
				if err != nil {
					t.Fatal(err)
				}
				return s, e
			}
			s, e := open()
			poll := func() {
				t.Helper()
				if ok, err := e.Poll(context.Background(), id); !ok || err != nil {
					t.Fatalf("poll: %v %v", ok, err)
				}
				now = now.Add(2 * time.Minute)
			}
			check := func(status, plan, email string, windows int) {
				t.Helper()
				if !publish {
					email = ""
				}
				before := calls.Load()
				w := httptest.NewRecorder()
				httpapi.NewUnauthenticated(s, clock).ServeHTTP(w, httptest.NewRequest("GET", "/v1/quotas", nil))
				var snap model.Snapshot
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &snap) != nil || len(snap.Subscriptions) != 1 || calls.Load() != before {
					t.Fatal("invalid or non-passive HTTP snapshot")
				}
				sub := snap.Subscriptions[0]
				matches := func(p *string, want string) bool { return p == nil && want == "" || p != nil && *p == want }
				if sub.Status != status || len(sub.Quotas) != windows || !matches(sub.Plan, plan) || !matches(sub.AccountEmail, email) {
					t.Fatalf("got status=%s windows=%d plan=%v email=%v; want %s/%d/%q/%q", sub.Status, len(sub.Quotas), sub.Plan, sub.AccountEmail, status, windows, plan, email)
				}
				if windows > 0 && sub.Quotas[0].ID != "gemini_5h" {
					t.Fatalf("primary window must be gemini_5h, got %s", sub.Quotas[0].ID)
				}
				for _, secret := range []string{"syn-token", "rt-unused", "acct-"} {
					if strings.Contains(w.Body.String(), secret) {
						t.Fatalf("leaked private credential field %s", secret)
					}
				}
			}

			check("unknown", "", "", 0)
			poll()
			check("ok", "Google AI Pro", "user1@example.test", 4)
			if profileCalls.Load() != 1 {
				t.Fatalf("expected 1 profile call, got %d", profileCalls.Load())
			}

			// Hourly OAuth access-token rotation for the same account reuses cached profile without re-calling loadCodeAssist.
			tokenGen.Store(2)
			poll()
			check("ok", "Google AI Pro", "user1@example.test", 4)
			if profileCalls.Load() != 1 {
				t.Fatalf("expected cached profile across token refresh, got %d profile calls", profileCalls.Load())
			}

			// Restart hides remote profile until the next live poll confirms identity.
			s.Close()
			s, e = open()
			check("ok", "", "", 4)
			poll()
			check("ok", "Google AI Pro", "user1@example.test", 4)

			// Expired token fails locally as auth_required, keeps last-good quotas stale, hides profile.
			mode.Store(2)
			poll()
			check("stale", "", "", 4)

			// Token refreshed externally -> recovers cleanly without losing identity.
			mode.Store(0)
			tokenGen.Store(3)
			poll()
			check("ok", "Google AI Pro", "user1@example.test", 4)

			// Switching to a different Google account on the same subscription ID is refused by the store
			// so quota history and metadata of two accounts are never mixed.
			account.Store(2)
			if ok, err := e.Poll(context.Background(), id); !ok || !errors.Is(err, store.ErrIdentityChanged) {
				t.Fatalf("expected ErrIdentityChanged on account switch, got ok=%v err=%v", ok, err)
			}
			check("stale", "", "", 4) // Old profile invalidated; never leaks account 1's email/plan.
		})
	}
}
