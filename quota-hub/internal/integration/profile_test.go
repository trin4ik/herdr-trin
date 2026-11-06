package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestProfilePipelinePrivacyAndInFlightSwitch(t *testing.T) {
	for _, publish := range []bool{false, true} {
		t.Run(fmt.Sprint(publish), func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			root := t.TempDir()
			auth := filepath.Join(root, "auth.json")
			writeAuth := func(account string) {
				t.Helper()
				b, _ := json.Marshal(map[string]any{"tokens": map[string]string{"access_token": "synthetic-access", "account_id": account}})
				if err := os.WriteFile(auth, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeAuth("account-a")
			var calls atomic.Int32
			var switchAccount atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if switchAccount.Load() {
					writeAuth("account-b")
				}
				fmt.Fprint(w, `{"account_id":"account-a","email":"person@example.test","plan_type":"prolite","rate_limit":{"primary_window":{"used_percent":32,"limit_window_seconds":604800}}}`)
			}))
			defer upstream.Close()
			def := model.Definition{ID: "codex", Provider: "codex", Source: "native_codex_oauth", StaleAfter: time.Hour, PublishAccountEmail: publish}
			spec := collectors.Spec{Definition: def, CredentialFile: auth, Interval: 5 * time.Minute, Timeout: time.Second}
			c, err := collectors.New(spec, collectors.Options{Now: clock, Endpoints: map[string]string{"codex": upstream.URL}})
			if err != nil {
				t.Fatal(err)
			}
			s, err := store.Open(filepath.Join(root, "state"), []model.Definition{def})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			workers := []engine.Worker{{Spec: spec, Collector: c}}
			e, err := engine.New(s, workers, engine.Options{Now: clock})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Poll(context.Background(), def.ID); err != nil {
				t.Fatal(err)
			}
			handler := httpapi.NewUnauthenticated(s, clock)
			read := func() model.Subscription {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/quotas", nil))
				var snap model.Snapshot
				if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &snap) != nil || snap.SchemaVersion != 1 || len(snap.Subscriptions) != 1 {
					t.Error("bad v1 response")
					return model.Subscription{}
				}
				return snap.Subscriptions[0]
			}
			sub := read()
			if sub.Plan == nil || *sub.Plan != "prolite" || (sub.AccountEmail != nil) != publish {
				t.Fatal("metadata pipeline/privacy failed")
			}
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); read() }()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatal("HTTP GET triggered upstream")
			}
			now = now.Add(6 * time.Minute)
			switchAccount.Store(true)
			if _, err := e.Poll(context.Background(), def.ID); err != nil {
				t.Fatal(err)
			}
			sub = read()
			if sub.AccountEmail != nil || sub.Plan != nil || len(sub.Quotas) != 1 {
				t.Fatal("in-flight switch leaked profile or lost usage")
			}
			// Returning to old credentials cannot resurrect a permanently invalidated profile.
			switchAccount.Store(false)
			writeAuth("account-a")
			now = now.Add(6 * time.Minute)
			if _, err := e.Poll(context.Background(), def.ID); err != nil {
				t.Fatal(err)
			}
			if sub = read(); sub.AccountEmail != nil || sub.Plan != nil {
				t.Fatal("old profile resurrected")
			}
			s.Close()
			s, err = store.Open(filepath.Join(root, "state"), []model.Definition{def})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err = engine.New(s, workers, engine.Options{Now: clock}); err != nil {
				t.Fatal(err)
			}
			handler = httpapi.NewUnauthenticated(s, clock)
			if sub = read(); sub.AccountEmail != nil || sub.Plan != nil {
				t.Fatal("restart resurrected profile")
			}
		})
	}
}
