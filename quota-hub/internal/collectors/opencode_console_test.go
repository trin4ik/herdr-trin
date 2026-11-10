package collectors

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quota-hub/internal/model"
)

// Synthetic Console answers. The real workspace is never read by tests.
const consoleCredentials = `{"data":[
  {"id":"cred_old","integrationID":"opencode","active":false,"value":{"type":"key","key":"sk-inactive-key"}},
  {"id":"cred_new","integrationID":"opencode","active":true,"value":{"type":"oauth","access":"st-console-session-token",
   "refresh":"rt-never-used","expires":1800000000000,"metadata":{"server":"https://opencode.ai/console",
   "orgID":"org_synthetic","orgName":"Personal","email":"nobody@example.invalid"}}}]}`

const consoleBilling = `{"billingMode":"prepaid","mode":"pay-as-you-go","balanceMicroCents":"1250000",
 "creditLimitMicroCents":"5000000","availableMicroCents":"1250000","canPurchaseCredits":true}`

const consoleModels = `{"items":[
 {"model":"minimax-m3.1-flash-free","provider":"opencode","subscription":false,"totalRequests":"121",
  "totalInputTokens":"314460","totalOutputTokens":"71236","totalCacheReadTokens":"13490246",
  "totalCacheWrite5mTokens":"0","totalCacheWrite1hTokens":"0","totalCostMicroCents":"0"},
 {"model":"glm-5.3-flash","provider":"opencode","subscription":true,"totalRequests":"12",
  "totalInputTokens":"100","totalOutputTokens":"20","totalCacheReadTokens":"0",
  "totalCacheWrite5mTokens":"0","totalCacheWrite1hTokens":"0","totalCostMicroCents":"15000"}],
 "pageInfo":{"page":1,"pageSize":100,"total":2,"pageCount":1}}`

const consoleBudgets = `[{"ruleID":"budget_team","limitDollars":20,"spentDollars":5,"scope":"each","per":"model",
 "coverage":"models","models":[{"providerID":"opencode","modelID":"glm-5.3-flash","name":"GLM"}],
 "vendors":[{"id":"zhipuai","name":"Zhipu"}],"picked":true,"shared":false,"resetsAt":"2026-12-01T00:00:00.000Z"}]`

const consoleGo = `{"usage":{"rolling":{"status":"ok","percent":12.5,"resetsAt":"2026-11-10T17:00:00.000Z"},
 "weekly":{"status":"rate-limited","percent":80,"resetsAt":"2026-11-13T07:00:00.000Z"},
 "monthly":{"status":"ok","percent":30,"resetsAt":"2026-12-01T00:00:00.000Z"}}}`

func consoleSpec(serviceFile, observedFile string) Spec {
	return Spec{
		Definition:          model.Definition{ID: "opencode", Provider: ProviderOpenCode, Source: SourceOpenCodeConsole, Product: "opencode_console"},
		IntegrationID:       OpenCodeConsoleIntegration,
		OpenCodeServiceFile: serviceFile,
		ObservedFile:        observedFile,
	}
}

// consoleHandler answers the Console API of the synthetic workspace.
func consoleHandler(t *testing.T, goStatus int, seen *[]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer st-console-session-token" {
			t.Errorf("console auth %q", r.Header.Get("Authorization"))
		}
		if seen != nil {
			*seen = append(*seen, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/billing/status":
			w.Write([]byte(consoleBilling))
		case "/api/usage/models":
			w.Write([]byte(consoleModels))
		case "/api/policies/current-budgets":
			w.Write([]byte(consoleBudgets))
		case "/zen/go/v1/usage":
			if goStatus != http.StatusOK {
				w.WriteHeader(goStatus)
				w.Write([]byte(`{"type":"error","error":{"type":"EntitlementError","message":"OpenCode Go subscription required."}}`))
				return
			}
			w.Write([]byte(consoleGo))
		default:
			t.Errorf("unexpected console path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func findQuota(quotas []model.Quota, id string) *model.Quota {
	for i := range quotas {
		if quotas[i].ID == id {
			return &quotas[i]
		}
	}
	return nil
}

func TestOpenCodeConsoleCollectsBalanceModelsBudgetsAndGo(t *testing.T) {
	oc := serve(t, openCodeHandler(t, []byte(consoleCredentials), basicFor(testServicePassword)))
	file := registrationFor(t, oc.URL, testServicePassword)
	var paths []string
	prov := serve(t, consoleHandler(t, http.StatusOK, &paths))
	c, err := New(consoleSpec(file, ""), opts(map[string]string{"console": prov.URL, "console_go": prov.URL + "/zen/go/v1/usage"}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceOpenCodeConsole {
		t.Fatalf("source %q", res.Source)
	}
	balance := model.Metric{}
	for _, m := range res.Metrics {
		if m.ID == "credits_balance" {
			balance = m
		}
	}
	if balance.Value != 1.25 || balance.Unit != "USD" {
		t.Fatalf("balance %+v", balance)
	}
	free := findQuota(res.Quotas, "model_minimax_m3_1_flash_free")
	if free == nil || free.Metric != "counter" || free.Unit != "requests" || free.Used == nil || *free.Used != 121 {
		t.Fatalf("free model counter %+v", free)
	}
	if free.Scope == nil || *free.Scope != "free" {
		t.Fatalf("free scope %+v", free.Scope)
	}
	if free.Counters["input_tokens"] != 314460 || free.Counters["cost_usd"] != 0 {
		t.Fatalf("counters %+v", free.Counters)
	}
	paid := findQuota(res.Quotas, "model_glm_5_3_flash")
	if paid == nil || paid.Scope != nil || paid.Counters["cost_usd"] != 0.015 {
		t.Fatalf("paid model %+v", paid)
	}
	budget := findQuota(res.Quotas, "budget_budget_team")
	if budget == nil || budget.UsedPercent == nil || *budget.UsedPercent != 25 {
		t.Fatalf("budget %+v", budget)
	}
	if budget.Counters["limit_usd"] != 20 || budget.Counters["spent_usd"] != 5 {
		t.Fatalf("budget counters %+v", budget.Counters)
	}
	weekly := findQuota(res.Quotas, "go_weekly")
	if weekly == nil || weekly.UsedPercent == nil || *weekly.UsedPercent != 80 {
		t.Fatalf("go weekly %+v", weekly)
	}
	if weekly.Limited == nil || !*weekly.Limited {
		t.Fatalf("go weekly limited %+v", weekly.Limited)
	}
	if findQuota(res.Quotas, "go_monthly") == nil || findQuota(res.Quotas, "go_rolling") == nil {
		t.Fatalf("go meters missing: %+v", res.Quotas)
	}
	for _, p := range paths {
		if strings.Contains(p, "credential") {
			t.Fatal("console request leaked to the credential path")
		}
	}
}

func TestOpenCodeConsoleWithoutGoIsNotAnError(t *testing.T) {
	oc := serve(t, openCodeHandler(t, []byte(consoleCredentials), basicFor(testServicePassword)))
	file := registrationFor(t, oc.URL, testServicePassword)
	prov := serve(t, consoleHandler(t, http.StatusForbidden, nil))
	c, err := New(consoleSpec(file, ""), opts(map[string]string{"console": prov.URL, "console_go": prov.URL + "/zen/go/v1/usage"}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if findQuota(res.Quotas, "go_weekly") != nil {
		t.Fatal("a workspace without Go must not report Go meters")
	}
	if findQuota(res.Quotas, "model_minimax_m3_1_flash_free") == nil {
		t.Fatal("model usage lost")
	}
}

func TestOpenCodeConsoleRefusesExpiredOrAmbiguousCredentials(t *testing.T) {
	oc := serve(t, openCodeHandler(t, []byte(consoleCredentials), basicFor(testServicePassword)))
	file := registrationFor(t, oc.URL, testServicePassword)
	prov := serve(t, consoleHandler(t, http.StatusOK, nil))
	expired := strings.Replace(consoleCredentials, `"expires":1800000000000`, `"expires":1700000000000`, 1)
	ocExpired := serve(t, openCodeHandler(t, []byte(expired), basicFor(testServicePassword)))
	ambiguous := strings.Replace(consoleCredentials, `"id":"cred_new"`, `"id":"cred_new2"`, 1)
	ambiguous = strings.Replace(ambiguous, `"active":false,"value":{"type":"key","key":"sk-inactive-key"}`,
		`"active":true,"value":{"type":"key","key":"sk-second-active-key"}`, 1)
	ocAmbiguous := serve(t, openCodeHandler(t, []byte(ambiguous), basicFor(testServicePassword)))

	for name, reg := range map[string]string{"expired": ocExpired.URL, "ambiguous": ocAmbiguous.URL} {
		f := registrationFor(t, reg, testServicePassword)
		c, err := New(consoleSpec(f, ""), opts(map[string]string{"console": prov.URL, "console_go": prov.URL + "/zen/go/v1/usage"}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Fetch(context.Background())
		if err == nil {
			t.Fatalf("%s credential accepted", name)
		}
		code := errInfo(t, err).Code
		if name == "expired" && code != CodeAuthRequired {
			t.Fatalf("expired: %s", code)
		}
		if name == "ambiguous" && code != CodeCredentialsUnavailable {
			t.Fatalf("ambiguous: %s", code)
		}
	}
	_ = file
}

func TestOpenCodeConsoleFreeLimitObservationKeepsRemoteWindows(t *testing.T) {
	obs := filepath.Join(t.TempDir(), "opencode.latest.json")
	body := `{"schema_version":1,"source":"opencode-http-response-hook","provider_id":"opencode",
	 "observed_at":"2026-11-04T11:55:00.000Z","status":429,
	 "quota":[{"name":"free_limit_reset","type":"time","family":"free_limit","value":"2026-11-05T00:00:00.000Z"}]}`
	if err := os.WriteFile(obs, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	oc := serve(t, openCodeHandler(t, []byte(consoleCredentials), basicFor(testServicePassword)))
	file := registrationFor(t, oc.URL, testServicePassword)
	prov := serve(t, consoleHandler(t, http.StatusOK, nil))
	c, err := New(consoleSpec(file, obs), opts(map[string]string{"console": prov.URL, "console_go": prov.URL + "/zen/go/v1/usage"}))
	if err != nil {
		t.Fatal(err)
	}
	h, ok := c.(HybridCollector)
	if !ok {
		t.Fatal("console collector must observe passively")
	}
	res, err := h.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if findQuota(res.Quotas, "model_minimax_m3_1_flash_free") == nil {
		t.Fatal("remote windows lost")
	}
	passive, err := h.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	free := findQuota(passive.Quotas, "free_models")
	if free == nil || free.UsedPercent == nil || *free.UsedPercent != 100 {
		t.Fatalf("free limit %+v", free)
	}
	if free.Window.ResetAt == nil || !free.Window.ResetAt.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("free reset %+v", free.Window)
	}
	if findQuota(passive.Quotas, "model_minimax_m3_1_flash_free") == nil {
		t.Fatal("passive observation dropped the remote windows")
	}
	if findQuota(passive.Quotas, "go_weekly") == nil {
		t.Fatal("passive observation dropped the Go meters")
	}
}

func TestOpenCodeConsoleNeverPublishesTheCredential(t *testing.T) {
	oc := serve(t, openCodeHandler(t, []byte(consoleCredentials), basicFor(testServicePassword)))
	file := registrationFor(t, oc.URL, testServicePassword)
	prov := serve(t, consoleHandler(t, http.StatusOK, nil))
	c, err := New(consoleSpec(file, ""), opts(map[string]string{"console": prov.URL, "console_go": prov.URL + "/zen/go/v1/usage"}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	for _, secret := range []string{"st-console-session-token", "rt-never-used", "sk-inactive-key", "nobody@example.invalid"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret %q leaked into the result", secret)
		}
	}
}
