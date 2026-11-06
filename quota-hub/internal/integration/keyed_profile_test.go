package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/engine"
	"quota-hub/internal/httpapi"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

func TestKeyedProfilesThroughCacheAndHTTP(t *testing.T) {
	for _, provider := range []string{"ollama", "kimi"} {
		for _, publish := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/publish=%v", provider, publish), func(t *testing.T) {
				now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
				clock := func() time.Time { return now }
				integration, plan := "ollama-cloud", "pro"
				if provider == "kimi" {
					integration, plan = "kimi-code-plan-cn", "Max"
				}
				var profileCalls, usageCalls atomic.Int32
				var profileStatus atomic.Int32
				profileStatus.Store(200)
				fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/credential":
						fmt.Fprintf(w, `{"data":[{"id":"selected","integrationID":%q,"active":true,"value":{"type":"key","key":"synthetic-key"}}]}`, integration)
					case "/usage":
						usageCalls.Add(1)
						if provider == "ollama" {
							fmt.Fprint(w, `{"included":{"weekly":{"remaining_percent":80}}}`)
						} else {
							fmt.Fprint(w, `{"usages":{"limit_5h":{"used_ratio":0.2}}}`)
						}
					case "/profile":
						profileCalls.Add(1)
						if code := int(profileStatus.Load()); code != 200 {
							w.WriteHeader(code)
							return
						}
						if provider == "ollama" {
							fmt.Fprint(w, `{"ID":"synthetic-account","Email":"person@example.test","Plan":"pro"}`)
						} else {
							fmt.Fprint(w, `{"user_id":"synthetic-account","email":"person@example.test","user_level_name":"Max","region":"REGION_OVERSEA"}`)
						}
					default:
						w.WriteHeader(404)
					}
				}))
				defer fake.Close()
				spec := collectors.Spec{Definition: model.Definition{ID: provider, Provider: provider, Source: "opencode_key", StaleAfter: time.Hour, PublishAccountEmail: publish}, IntegrationID: integration, OpenCodeURL: fake.URL, Interval: 5 * time.Minute, Timeout: time.Second}
				c, err := collectors.New(spec, collectors.Options{Now: clock, Endpoints: map[string]string{"opencode": fake.URL, provider: fake.URL + "/usage", provider + "_profile": fake.URL + "/profile"}})
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(t.TempDir(), "store")
				s, err := store.Open(dir, []model.Definition{spec.Definition})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { s.Close() }()
				workers := []engine.Worker{{Spec: spec, Collector: c}}
				e, err := engine.New(s, workers, engine.Options{Now: clock})
				if err != nil {
					t.Fatal(err)
				}
				poll := func() {
					t.Helper()
					if ok, err := e.Poll(context.Background(), provider); !ok || err != nil {
						t.Fatalf("poll %v %v", ok, err)
					}
				}
				poll()
				check := func() {
					t.Helper()
					sub, _ := s.GetAt(provider, now)
					if sub.Plan == nil || *sub.Plan != plan || (sub.AccountEmail != nil) != publish || sub.Status != "ok" || len(sub.Quotas) != 1 {
						t.Fatal("metadata/usage/privacy pipeline failed")
					}
				}
				check()
				if profileCalls.Load() != 1 {
					t.Fatal("initial profile missing")
				}
				now = now.Add(6 * time.Minute)
				poll()
				check()
				if profileCalls.Load() != 1 || usageCalls.Load() != 2 {
					t.Fatal("profile TTL bypassed or usage cadence changed")
				}
				// Concurrent unauthenticated GETs never reach either upstream endpoint.
				handler := httpapi.NewUnauthenticated(s, clock)
				var wg sync.WaitGroup
				for i := 0; i < 20; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						w := httptest.NewRecorder()
						handler.ServeHTTP(w, httptest.NewRequest("GET", "/v1/quotas", nil))
						var snap model.Snapshot
						if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &snap) != nil || snap.SchemaVersion != 1 {
							t.Error("HTTP contract")
						}
					}()
				}
				wg.Wait()
				if profileCalls.Load() != 1 || usageCalls.Load() != 2 {
					t.Fatal("HTTP triggered upstream")
				}
				s.Close()
				s, err = store.Open(dir, []model.Definition{spec.Definition})
				if err != nil {
					t.Fatal(err)
				}
				e, err = engine.New(s, workers, engine.Options{Now: clock})
				if err != nil {
					t.Fatal(err)
				}
				if sub, _ := s.GetAt(provider, now); sub.AccountEmail != nil || sub.Plan != nil {
					t.Fatal("restart served unverified cached credential profile")
				}
				now = now.Add(6 * time.Minute)
				poll()
				check()
				if profileCalls.Load() != 1 {
					t.Fatal("restart erased metadata cache schedule")
				}
				// Expired metadata cache failure must not turn successful usage into stale.
				now = now.Add(7 * time.Hour)
				profileStatus.Store(500)
				poll()
				check()
				if profileCalls.Load() != 2 {
					t.Fatal("profile never refreshed after TTL")
				}
				state, ok := s.RemoteProfileSchedule(provider)
				if !ok || state.Error == nil || state.Error.HTTPStatus != 500 {
					t.Fatal("profile error not recorded separately")
				}
			})
		}
	}
}
