package collectors

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"quota-hub/internal/model"
)

// Synthetic fixtures only; tests never read a real workspace or credential.
const consoleCredentials = `{"data":[
 {"integrationID":"opencode","active":false,"value":{"type":"key","key":"sk-inactive-key"}},
 {"id":"cred-synthetic","integrationID":"opencode","active":true,"value":{"type":"oauth","methodID":"device","access":"st-console-session-token",
 "refresh":"rt-never-used","expires":1800000000000,"metadata":{"server":"https://opencode.ai/console","accountID":"user-synthetic","orgID":"org_synthetic","email":"nobody@example.invalid"}}}]}`

const consoleGo = `{"usage":{"rolling":{"status":"ok","percent":12.5,"resetsAt":"2026-11-10T17:00:00.000Z"},
 "weekly":{"status":"rate-limited","percent":80,"resetsAt":"2026-11-13T07:00:00.000Z"},
 "monthly":{"status":"ok","percent":30,"resetsAt":"2026-12-01T00:00:00.000Z"}}}`

func findQuota(quotas []model.Quota, id string) *model.Quota {
	for i := range quotas {
		if quotas[i].ID == id {
			return &quotas[i]
		}
	}
	return nil
}

func consoleSpec(serviceFile string) Spec {
	return Spec{
		Definition:    model.Definition{ID: "opencode", Provider: ProviderOpenCode, Source: SourceOpenCodeConsole, Product: "opencode_go"},
		IntegrationID: OpenCodeConsoleIntegration, OpenCodeServiceFile: serviceFile,
	}
}

func newGoForTest(t *testing.T, credentials string, handler http.HandlerFunc) Collector {
	t.Helper()
	return newGoWithConsentForTest(t, credentials, false, handler)
}

func newGoWithConsentForTest(t *testing.T, credentials string, publish bool, handler http.HandlerFunc) Collector {
	t.Helper()
	oc := serve(t, openCodeHandler(t, []byte(credentials), basicFor(testServicePassword)))
	prov := serve(t, handler)
	spec := consoleSpec(registrationFor(t, oc.URL, testServicePassword))
	spec.PublishAccountEmail = publish
	c, err := New(spec, opts(map[string]string{"console_go": prov.URL + "/zen/go/v1/usage"}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOpenCodeGoOnlyRequestsAllowance(t *testing.T) {
	requests := 0
	c := newGoForTest(t, consoleCredentials, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || r.URL.Path != "/zen/go/v1/usage" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer st-console-session-token" ||
			r.Header.Get("x-org-id") != "org_synthetic" || r.Header.Get("x-opencode-org-id") != "org_synthetic" {
			t.Error("incorrect credential/workspace selection")
		}
		w.Write([]byte(consoleGo))
	})
	if _, hybrid := c.(HybridCollector); hybrid {
		t.Fatal("Go must not observe legacy free refusals")
	}
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(res.Quotas) != 3 || len(res.Metrics) != 0 || res.Source != SourceOpenCodeConsole {
		t.Fatalf("unexpected snapshot %+v, requests=%d", res, requests)
	}
	if res.Plan == nil || *res.Plan != "Go" || res.Profile != nil {
		t.Fatal("paid meter must confirm Go, but email export requires consent")
	}
	for i, id := range []string{"go_rolling", "go_weekly", "go_monthly"} {
		q := res.Quotas[i]
		if q.ID != id || q.Limited == nil || !*q.Limited || q.RemainingPercent == nil ||
			*q.RemainingPercent != 100-*q.UsedPercent || q.ObservedAt == nil || q.Window.ResetAt == nil {
			t.Fatalf("invalid meter %+v", q)
		}
	}
	if *res.Quotas[0].Window.DurationSeconds != 18000 || *res.Quotas[1].Window.Period != "week" || *res.Quotas[2].Window.Period != "month" {
		t.Fatal("incorrect window lengths")
	}
	raw, _ := json.Marshal(res)
	for _, secret := range []string{"st-console-session-token", "rt-never-used", "sk-inactive-key", "nobody@example.invalid", "org_synthetic"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("credential metadata leaked")
		}
	}
}

func TestOpenCodeGoRejectsExpiredAmbiguousAndMissingCredentials(t *testing.T) {
	for name, credentials := range map[string]string{
		"expired":   strings.Replace(consoleCredentials, `1800000000000`, `1700000000000`, 1),
		"ambiguous": strings.Replace(consoleCredentials, `"active":false`, `"active":true`, 1),
		"missing":   `{"data":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newGoForTest(t, credentials, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream request") })
			res, err := c.Fetch(context.Background())
			if err == nil || res.Plan != nil || res.Profile != nil {
				t.Fatal("invalid credential returned metadata")
			}
			want := CodeCredentialsUnavailable
			if name == "expired" {
				want = CodeAuthRequired
			}
			if errInfo(t, err).Code != want {
				t.Fatalf("wrong error: %v", err)
			}
		})
	}
}

func TestOpenCodeGoRequiresEntitlementAndPropagatesRateLimit(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500} {
		c := newGoForTest(t, consoleCredentials, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(status)
		})
		res, err := c.Fetch(context.Background())
		if err == nil || len(res.Quotas) != 0 || res.Plan != nil || res.Profile != nil || errInfo(t, err).HTTPStatus != status {
			t.Fatalf("status %d became a valid/empty allowance: %+v %v", status, res, err)
		}
		if status == 429 && errInfo(t, err).RetryAt == nil {
			t.Fatal("Retry-After lost")
		}
	}
}

func TestOpenCodeGoInvalidAndPartialSuccessIsNotZero(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"usage":{}}`,
		strings.Replace(consoleGo, `"percent":12.5`, `"percent":null`, 1),
		strings.Replace(consoleGo, `"percent":12.5`, `"percent":-1`, 1),
		strings.Replace(consoleGo, `"percent":12.5`, `"percent":101`, 1),
		strings.Replace(consoleGo, `"status":"ok"`, `"status":"unknown"`, 1),
		strings.Replace(consoleGo, `"resetsAt":"2026-11-10T17:00:00.000Z"`, `"resetsAt":null`, 1),
		strings.Replace(consoleGo, `"monthly":`, `"missing":`, 1),
	} {
		c := newGoForTest(t, consoleCredentials, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
		res, err := c.Fetch(context.Background())
		if err == nil || errInfo(t, err).Code != CodeInvalidResponse || len(res.Quotas) != 0 || res.Plan != nil || res.Profile != nil {
			t.Fatalf("invalid success accepted: %s, %+v %v", body, res, err)
		}
	}
}

func TestOpenCodeGoEmailSource(t *testing.T) {
	for _, tc := range []struct {
		name, credentials string
		wantEmail         bool
	}{
		{"official device", consoleCredentials, true},
		{"old official host", strings.Replace(consoleCredentials, "https://opencode.ai/console", "https://console.opencode.ai", 1), true},
		{"custom Console", strings.Replace(consoleCredentials, "https://opencode.ai/console", "https://console.example.test", 1), false},
		{"unknown method", strings.Replace(consoleCredentials, `"device"`, `"other"`, 1), false},
		{"no account ID", strings.Replace(consoleCredentials, `"user-synthetic"`, `null`, 1), false},
		{"bad email", strings.Replace(consoleCredentials, `"nobody@example.invalid"`, `{"address":"nobody@example.invalid"}`, 1), false},
		{"key", `{"data":[{"id":"c1","integrationID":"opencode","active":true,"value":{"type":"key","key":"synthetic-key","metadata":{"server":"https://opencode.ai/console","orgID":"org_synthetic","accountID":"user-synthetic","email":"nobody@example.invalid"}}}]}`, false},
		{"inactive metadata", `{"data":[{"id":"c1","integrationID":"opencode","active":false,"value":{"type":"oauth","methodID":"device","access":"unused","metadata":{"server":"https://opencode.ai/console","orgID":"org_synthetic","accountID":"user-synthetic","email":"nobody@example.invalid"}}},{"id":"c2","integrationID":"opencode","active":true,"label":"nobody@example.invalid","value":{"type":"oauth","methodID":"device","access":"active"}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGoWithConsentForTest(t, tc.credentials, true, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(consoleGo))
			})
			res, err := c.Fetch(context.Background())
			if err != nil || res.Plan == nil || *res.Plan != "Go" {
				t.Fatalf("metadata must not affect a valid meter: %v", err)
			}
			if tc.wantEmail {
				if res.Profile == nil || res.Profile.AccountEmail == nil || *res.Profile.AccountEmail != "nobody@example.invalid" ||
					res.Profile.Identity != res.ProfileIdentity || res.ProfileIdentity == noConsoleProfile {
					t.Fatal("missing bound Console email")
				}
			} else if res.Profile != nil || res.ProfileIdentity != noConsoleProfile {
				t.Fatal("unverified Console email accepted")
			}
		})
	}
}

func TestOpenCodeGoZeroAndFullAreFiniteAllowances(t *testing.T) {
	for _, value := range []string{"0", "100"} {
		body := strings.Replace(consoleGo, `"percent":12.5`, `"percent":`+value, 1)
		quotas, err := parseConsoleGo([]byte(body), fakeNow)
		if err != nil || len(quotas) != 3 || !*quotas[0].Limited {
			t.Fatalf("%s: %+v %v", value, quotas, err)
		}
	}
	quotas, err := parseConsoleGo([]byte(strings.ReplaceAll(consoleGo, `"status":"ok",`, "")), fakeNow)
	if err != nil || !*quotas[0].Limited {
		t.Fatal("absence of status cannot imply unlimited")
	}
}
