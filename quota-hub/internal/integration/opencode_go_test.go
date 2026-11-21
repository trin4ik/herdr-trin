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

func TestOpenCodeGoProfilePublication(t *testing.T) {
	for _, publish := range []bool{false, true} {
		t.Run(fmt.Sprintf("publish=%v", publish), func(t *testing.T) {
			var account, mode, calls atomic.Int32
			account.Store(1)
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.RawQuery != "" {
					t.Error("unexpected OpenCode request")
				}
				switch r.URL.Path {
				case "/api/credential":
					n := account.Load()
					fmt.Fprintf(w, `{"data":[{"id":"same-entry","integrationID":"opencode","active":true,"value":{"type":"oauth","methodID":"device","access":"synthetic-token-%d","metadata":{"server":"https://opencode.ai/console","orgID":"org-synthetic","accountID":"account-%d","email":"person%d@example.test"}}}]}`, n, n, n)
				case "/zen/go/v1/usage":
					if r.Header.Get("Authorization") != fmt.Sprintf("Bearer synthetic-token-%d", account.Load()) {
						t.Error("wrong active token for usage")
					}
					switch mode.Load() {
					case 1:
						account.Store(2) // Switch with the request in flight, reusing the entry ID.
					case 2:
						w.WriteHeader(403)
						return
					}
					fmt.Fprint(w, `{"usage":{"rolling":{"percent":10},"weekly":{"percent":20},"monthly":{"percent":30}}}`)
				default:
					t.Error("unexpected endpoint")
					w.WriteHeader(404)
				}
			}))
			defer fake.Close()
			dir := t.TempDir()
			reg := filepath.Join(dir, "service.json")
			if err := os.WriteFile(reg, []byte(fmt.Sprintf(`{"url":%q}`, fake.URL)), 0600); err != nil {
				t.Fatal(err)
			}
			id := "opencode-go"
			spec := collectors.Spec{Definition: model.Definition{
				ID: id, Provider: collectors.ProviderOpenCode, Source: collectors.SourceOpenCodeConsole,
				Product: "opencode_go", PublishAccountEmail: publish, StaleAfter: time.Hour,
			}, IntegrationID: collectors.OpenCodeConsoleIntegration, OpenCodeServiceFile: reg, Interval: time.Minute, Timeout: time.Second}
			now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			c, err := collectors.New(spec, collectors.Options{Now: clock, Endpoints: map[string]string{"console_go": fake.URL + "/zen/go/v1/usage"}})
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
				now = now.Add(time.Minute)
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
					t.Fatalf("wrong public status, plan or email: want %s/%s/%s", status, plan, email)
				}
				for _, private := range []string{"synthetic-token", "same-entry", "org-synthetic", "account-"} {
					if strings.Contains(w.Body.String(), private) {
						t.Fatal("private credential data in HTTP snapshot")
					}
				}
			}
			check("unknown", "", "", 0)
			poll()
			check("ok", "Go", "person1@example.test", 3)
			s.Close()
			s, e = open()
			check("ok", "Go", "", 3) // No cached email before revalidation on startup.
			poll()
			check("ok", "Go", "person1@example.test", 3)
			mode.Store(1)
			poll()
			check("ok", "Go", "", 3) // Old email suppressed on a switch during usage.
			mode.Store(0)
			poll()
			check("ok", "Go", "person2@example.test", 3)
			mode.Store(2)
			poll()
			check("stale", "", "", 3) // Keep last-good usage, not active entitlement or email.
		})
	}
}
