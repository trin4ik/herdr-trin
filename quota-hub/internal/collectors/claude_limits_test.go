package collectors

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"quota-hub/internal/model"
)

// A synthetic usage response in the current shape: the extensible limits[] array
// plus the legacy flat keys, including the model-scoped weekly limit.
const claudeLimitsBody = `{
 "five_hour": {"utilization": 100, "resets_at": "2026-11-10T13:00:00.147578+00:00", "limit_dollars": null},
 "seven_day": {"utilization": 41, "resets_at": "2026-11-13T07:00:00.147596+00:00"},
 "seven_day_opus": null,
 "seven_day_sonnet": null,
 "limits": [
  {"kind": "session", "group": "session", "percent": 100, "severity": "critical",
   "resets_at": "2026-11-10T13:00:00.147578+00:00", "scope": null, "is_active": true},
  {"kind": "weekly_all", "group": "weekly", "percent": 41, "severity": "normal",
   "resets_at": "2026-11-13T07:00:00.147596+00:00", "scope": null, "is_active": false},
  {"kind": "weekly_scoped", "group": "weekly", "percent": 10, "severity": "normal",
   "resets_at": "2026-11-13T07:00:00.147738+00:00",
   "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": false}],
 "spend": {"percent": 0, "enabled": 0},
 "weekly_scoped_shares": [{"limit_index": 2, "used_percent_of_weekly": 5, "allowance_percent_of_weekly": 50}]
}`

func TestClaudeUsageReadsEveryWindowOfTheLimitsArray(t *testing.T) {
	res, err := parseClaudeUsage([]byte(claudeLimitsBody), fakeNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Quotas) != 3 {
		t.Fatalf("windows %+v", res.Quotas)
	}
	session := quotaByID(t, res, "five_hour")
	if session.UsedPercent == nil || *session.UsedPercent != 100 {
		t.Fatalf("session %+v", session)
	}
	if session.Window.DurationSeconds == nil || *session.Window.DurationSeconds != 18000 {
		t.Fatalf("session window %+v", session.Window)
	}
	weekly := quotaByID(t, res, "seven_day")
	if weekly.UsedPercent == nil || *weekly.UsedPercent != 41 {
		t.Fatalf("weekly %+v", weekly)
	}
	scoped := quotaByID(t, res, "seven_day_fable")
	if scoped.UsedPercent == nil || *scoped.UsedPercent != 10 {
		t.Fatalf("scoped %+v", scoped)
	}
	if scoped.Scope == nil || *scoped.Scope != "Fable" {
		t.Fatalf("scoped scope %+v", scoped.Scope)
	}
	if scoped.Window.Period == nil || *scoped.Window.Period != "week" {
		t.Fatalf("scoped window %+v", scoped.Window)
	}
	if scoped.ObservedAt == nil || !scoped.ObservedAt.Equal(fakeNow) {
		t.Fatalf("scoped observed at %+v", scoped.ObservedAt)
	}
}

func TestClaudeUsageFallsBackToFlatKeysWithoutLimits(t *testing.T) {
	res, err := parseClaudeUsage([]byte(`{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`), fakeNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Quotas) != 2 {
		t.Fatalf("windows %+v", res.Quotas)
	}
	if q := quotaByID(t, res, "five_hour"); q.UsedPercent == nil || *q.UsedPercent != 12 {
		t.Fatalf("five hour %+v", q)
	}
}

func TestClaudeUsageKeepsUnknownKindsAsTheirOwnWindow(t *testing.T) {
	body := `{"limits":[{"kind":"monthly_all","group":"monthly","percent":7,"resets_at":"2026-12-01T00:00:00Z","scope":null}]}`
	res, err := parseClaudeUsage([]byte(body), fakeNow)
	if err != nil {
		t.Fatal(err)
	}
	q := quotaByID(t, res, "monthly_all_monthly")
	if q.UsedPercent == nil || *q.UsedPercent != 7 {
		t.Fatalf("unknown kind %+v", q)
	}
	if q.Window.Period == nil || *q.Window.Period != "month" {
		t.Fatalf("unknown kind window %+v", q.Window)
	}
}

func TestClaudeHybridKeepsTheScopedLimitWhenThePassiveObservationArrives(t *testing.T) {
	obs := filepath.Join(t.TempDir(), "claude.latest.json")
	if err := writeFile(obs, `{"schema_version":1,"source":"opencode-http-response-hook","provider_id":"anthropic",
	 "observed_at":"2026-11-04T12:00:30.000Z","status":200,"quota":[
	 {"name":"anthropic-ratelimit-unified-5h-utilization","type":"number","family":"subscription_candidate","value":0.9},
	 {"name":"anthropic-ratelimit-unified-7d-utilization","type":"number","family":"subscription_candidate","value":0.4}]}`); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(t.TempDir(), "credentials.json")
	if err := writeFile(creds, `{"claudeAiOauth":{"accessToken":"synthetic-access","expiresAt":1800000000000}}`); err != nil {
		t.Fatal(err)
	}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(claudeLimitsBody))
	})
	spec := hybridSpec(creds, obs)
	c, err := New(spec, opts(map[string]string{"claude": srv.URL}))
	if err != nil {
		t.Fatal(err)
	}
	h := c.(HybridCollector)
	native, err := h.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if findQuota(native.Quotas, "seven_day_fable") == nil {
		t.Fatalf("native windows %+v", native.Quotas)
	}
	passive, err := h.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if findQuota(passive.Quotas, "seven_day_fable") == nil {
		t.Fatalf("the passive observation dropped the scoped limit: %+v", passive.Quotas)
	}
	session := findQuota(passive.Quotas, "five_hour")
	if session == nil || session.UsedPercent == nil || *session.UsedPercent != 90 {
		t.Fatalf("the passive value must win for a window both sources report: %+v", session)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

func TestWindowSetKeepsAStableOrder(t *testing.T) {
	var set windowSet
	older := fakeNow.Add(-time.Minute)
	newer := fakeNow
	set.setRemote([]model.Quota{
		{ID: "five_hour", UsedPercent: fptr(100), ObservedAt: &newer},
		{ID: "seven_day", UsedPercent: fptr(41), ObservedAt: &newer},
		{ID: "seven_day_fable", UsedPercent: fptr(10), ObservedAt: &newer},
	})
	set.setObserved([]model.Quota{
		{ID: "five_hour", UsedPercent: fptr(90), ObservedAt: &newer},
		{ID: "free_models", UsedPercent: fptr(100), ObservedAt: &older},
	})
	all := set.all()
	if len(all) != 4 {
		t.Fatalf("windows %+v", all)
	}
	if all[0].ID != "five_hour" || all[1].ID != "seven_day" || all[2].ID != "seven_day_fable" {
		t.Fatalf("order %+v", all)
	}
	if all[0].UsedPercent == nil || *all[0].UsedPercent != 90 {
		t.Fatalf("the newer value must win: %+v", all[0])
	}
	if all[3].ID != "free_models" {
		t.Fatalf("an observed-only window must be kept: %+v", all[3])
	}
}
