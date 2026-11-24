package usage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	quotaModel "quota-hub/internal/model"
)

func pointer[T any](value T) *T { return &value }

func TestQuotaInstances(t *testing.T) {
	i := importer(t)
	now := clock
	i.Now = func() time.Time { return now }
	reset := clock.Add(time.Hour)
	duration := int64(18000)
	snapshot := quotaModel.Snapshot{SchemaVersion: 1, Subscriptions: []quotaModel.Subscription{{
		ID: "claude", OpenCodeProvider: pointer("anthropic"),
		Quotas: []quotaModel.Quota{
			{ID: "all", UsedPercent: pointer(42.0), Window: quotaModel.Window{ResetAt: &reset, DurationSeconds: &duration}},
			{ID: "fable", Model: pointer("Fable"), Window: quotaModel.Window{ResetAt: &reset, DurationSeconds: &duration}},
			{ID: "monthly", Window: quotaModel.Window{ResetAt: &reset, Period: pointer("month")}},
			{ID: "no-reset"},
		},
	}, {ID: "unbound", Quotas: []quotaModel.Quota{{ID: "ignored", Window: quotaModel.Window{ResetAt: &reset}}}}}}
	status := http.StatusOK
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
			t.Error("expected unauthenticated GET")
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(snapshot)
	}))
	defer server.Close()
	i.QuotasURL = server.URL
	addEvent := func(id, provider, name string, at time.Time) string {
		var e Event
		json.Unmarshal([]byte(event(id, at)), &e)
		e.Provider, e.Model = &provider, &name
		b, _ := json.Marshal(e)
		return string(b) + "\n"
	}
	write(t, filepath.Join(i.Dir, "events", "a.jsonl"),
		addEvent("fable", "anthropic", "claude-fable-5-2", clock.Add(-time.Hour))+
			addEvent("other", "anthropic", "claude-other", clock.Add(-time.Hour))+
			addEvent("unmatched", "openai", "claude-fable-5-2", clock.Add(-time.Hour))+
			addEvent("old", "openai", "m", clock.Add(-25*time.Hour))+
			addEvent("boundary", "anthropic", "claude-fable-5-2", reset))
	first := cycle(t, i)
	if !first.OK || first.Quotas.Error != nil || len(first.Current) != 3 || first.Unmatched.Calls != 1 {
		t.Fatalf("%+v", first)
	}
	if first.Current[0].Calls != 2 || first.Current[0].UsedPercent == nil || *first.Current[0].UsedPercent != 42 ||
		first.Current[1].Calls != 1 || first.Current[1].Tokens.Input != 10 ||
		!first.Current[2].Partial || first.Current[2].Start != nil {
		t.Fatalf("%+v", first.Current)
	}
	// Positive and negative jitter do not move the recorded boundary.
	for _, offset := range []time.Duration{5 * time.Second, -5 * time.Second, 120 * time.Second, -120 * time.Second} {
		mu.Lock()
		reset = clock.Add(time.Hour + offset)
		mu.Unlock()
		cycle(t, i)
	}
	instances, err := readQuotaWindows(i.Dir)
	if err != nil || len(instances) != 3 {
		t.Fatalf("%+v %v", instances, err)
	}
	if instances[0].Start == nil || !instances[0].Start.Equal(clock.Add(-4*time.Hour)) {
		t.Fatal(instances[0])
	}
	// New instance; unknown duration starts at the preceding reset, never 30 days.
	mu.Lock()
	reset = clock.Add(2 * time.Hour)
	mu.Unlock()
	now = clock.Add(time.Hour)
	next := cycle(t, i)
	if next.Current[2].Partial || next.Current[2].Start == nil || !next.Current[2].Start.Equal(clock.Add(time.Hour)) {
		t.Fatalf("%+v", next.Current[2])
	}
	instances, err = readQuotaWindows(i.Dir)
	if err != nil || len(instances) != 6 {
		t.Fatalf("%+v %v", instances, err)
	}
	// Restart with a stale state: journal recovery must not duplicate appends.
	if err := saveJSON(filepath.Join(i.Dir, "state.json"), state{}); err != nil {
		t.Fatal(err)
	}
	cycle(t, i)
	instances, _ = readQuotaWindows(i.Dir)
	if len(instances) != 6 {
		t.Fatal(len(instances))
	}
	mu.Lock()
	status = http.StatusServiceUnavailable
	mu.Unlock()
	failed := cycle(t, i)
	if !failed.OK || failed.Quotas.Error == nil || len(failed.Current) != 3 || failed.Unmatched == nil ||
		failed.Current[0].UsedPercent == nil {
		t.Fatalf("%+v", failed)
	}
	reports, err := ReadWindows(i.Dir)
	if err != nil || len(reports) != 6 || reports[1].Calls != 1 {
		t.Fatalf("%+v %v", reports, err)
	}
	b, _ := json.Marshal(reports)
	if strings.Contains(string(b), "usedPercent") {
		t.Fatal(string(b))
	}
	// Rebinding the subscription to another OpenCode provider re-records the
	// current windows, so calls of the new provider count from now on.
	write(t, filepath.Join(i.Dir, "events", "b.jsonl"), addEvent("agent", "claude-agent", "opus", now.Add(-10*time.Minute)))
	mu.Lock()
	status = http.StatusOK
	snapshot.Subscriptions[0].OpenCodeProvider = pointer("claude-agent")
	mu.Unlock()
	rebound := cycle(t, i)
	instances, _ = readQuotaWindows(i.Dir)
	if len(instances) != 9 || rebound.Current[0].Provider != "claude-agent" || rebound.Current[0].Calls != 1 {
		t.Fatalf("%d %+v", len(instances), rebound.Current)
	}
	cycle(t, i)
	if instances, _ = readQuotaWindows(i.Dir); len(instances) != 9 {
		t.Fatal(len(instances))
	}
}

func TestQuotaPartialAndModelMatching(t *testing.T) {
	i := importer(t)
	start, reset := clock.Add(-time.Hour), clock.Add(time.Hour)
	records := []Record{
		{Event: Event{Time: start.Add(-time.Millisecond).UnixMilli(), Provider: pointer("anthropic"), Model: pointer("claude-fable-5-2")}},
		{Event: Event{Time: start.UnixMilli(), Provider: pointer("anthropic"), Model: pointer("claude-fable-5-2")}, USD: pointer(0.25)},
		{Event: Event{Time: clock.UnixMilli(), Provider: pointer("anthropic"), Model: pointer("claude-fable-5-2")}},
		{Event: Event{Time: clock.UnixMilli(), Provider: pointer("anthropic"), Model: pointer("claude-fablex")}},
		{Event: Event{Time: clock.UnixMilli(), Provider: pointer("openai"), Model: pointer("claude-fable-5-2")}},
		{Event: Event{Time: reset.UnixMilli(), Provider: pointer("anthropic"), Model: pointer("claude-fable-5-2")}},
	}
	for _, record := range records {
		b, _ := json.Marshal(record)
		if err := appendLedger(filepath.Join(i.Dir, "ledger", day(record.Time)+".jsonl"), append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	window := QuotaWindow{Provider: "anthropic", Model: pointer(" --Fable!! "), SeenAt: start, Reset: reset}
	report, err := reportQuotaWindow(i.Dir, window)
	if err != nil || report.Calls != 2 || report.USD != 0.25 || report.Unpriced != 1 || !report.Partial {
		t.Fatalf("%+v %v", report, err)
	}
	window.Start, window.Model = &start, nil
	report, err = reportQuotaWindow(i.Dir, window)
	if err != nil || report.Calls != 3 || report.Partial {
		t.Fatalf("%+v %v", report, err)
	}
}

func TestQuotaFailuresAndDisabled(t *testing.T) {
	for _, body := range []string{`bad json`, `{"schema_version":99}`, strings.Repeat("x", (1<<20)+1)} {
		t.Run(body[:8], func(t *testing.T) {
			i := importer(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(body))
			}))
			defer server.Close()
			i.QuotasURL = server.URL
			s := cycle(t, i)
			if !s.OK || s.Quotas.Error == nil || s.Unmatched != nil {
				t.Fatalf("%+v", s)
			}
		})
	}
	i := importer(t)
	s := cycle(t, i)
	if s.Quotas != nil || s.Current != nil || s.Unmatched != nil {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(filepath.Join(i.Dir, "windows.jsonl")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestQuotaWindowSumsAnthropicAndClaudeAgent(t *testing.T) {
	i := importer(t)
	var err error
	i.prices, err = decodePrices([]byte(`{"anthropic":{"models":{
		"claude-opus-5-5":{"canonical_model_id":"anthropic/claude-opus-5-5","release_date":"2026-09-22","cost":{"input":4}}
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	reset := clock.Add(time.Hour)
	duration := int64(18000)
	snapshot := quotaModel.Snapshot{SchemaVersion: 1, Subscriptions: []quotaModel.Subscription{{
		ID: "claude", OpenCodeProvider: pointer("anthropic"), OpenCodeProviders: []string{"anthropic", "claude-agent"},
		Quotas: []quotaModel.Quota{{ID: "five_hour", Window: quotaModel.Window{ResetAt: &reset, DurationSeconds: &duration}}},
	}}}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(snapshot)
	}))
	defer server.Close()
	i.QuotasURL = server.URL
	add := func(id, provider, name string) string {
		e := Event{V: 1, ID: id, Time: clock.Add(-time.Hour).UnixMilli(), Source: "step", Status: "ended", Session: "s",
			Provider: &provider, Model: &name, Tokens: Tokens{Input: 1e6}}
		b, _ := json.Marshal(e)
		return string(b) + "\n"
	}
	write(t, filepath.Join(i.Dir, "events", "a.jsonl"),
		add("api", "anthropic", "claude-opus-5-5")+add("agent", "claude-agent", "opus")+add("other", "openai", "gpt"))
	first := cycle(t, i)
	if len(first.Current) != 1 {
		t.Fatalf("%+v", first.Current)
	}
	w := first.Current[0]
	// One total for the account; each call priced by its own provider's rules.
	if w.Calls != 2 || w.USD != 8 || w.Unpriced != 0 || len(w.Providers) != 2 || first.Unmatched.Calls != 1 {
		t.Fatalf("%+v unmatched %+v", w, first.Unmatched)
	}
	// Narrowing the binding re-records the window with one provider.
	mu.Lock()
	snapshot.Subscriptions[0].OpenCodeProviders = []string{"claude-agent"}
	snapshot.Subscriptions[0].OpenCodeProvider = pointer("claude-agent")
	mu.Unlock()
	next := cycle(t, i)
	if next.Current[0].Calls != 1 || next.Current[0].Provider != "claude-agent" || len(next.Current[0].Providers) != 0 {
		t.Fatalf("%+v", next.Current[0])
	}
	if instances, _ := readQuotaWindows(i.Dir); len(instances) != 2 {
		t.Fatal(len(instances))
	}
}

func TestQuotaWindowSeparatesGooglePoolsByModelList(t *testing.T) {
	i := importer(t)
	start, reset := clock.Add(-time.Hour), clock.Add(time.Hour)
	records := []Record{
		{Event: Event{Time: start.UnixMilli(), Provider: pointer("google"), Model: pointer("antigravity-gemini-4-neon-high")}, USD: pointer(1.25)},
		{Event: Event{Time: start.UnixMilli(), Provider: pointer("google"), Model: pointer("antigravity-gemini-4-argon-high")}, USD: pointer(0.50)},
		{Event: Event{Time: start.UnixMilli(), Provider: pointer("google"), Model: pointer("antigravity-claude-opus-5-5-thinking")}, USD: pointer(2.00)},
		{Event: Event{Time: start.UnixMilli(), Provider: pointer("google"), Model: pointer("antigravity-gpt-oss-120b-medium")}},
	}
	for _, record := range records {
		b, _ := json.Marshal(record)
		if err := appendLedger(filepath.Join(i.Dir, "ledger", day(record.Time)+".jsonl"), append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}

	gemini, err := reportQuotaWindow(i.Dir, QuotaWindow{Provider: "google", Model: pointer("gemini"), Start: &start, Reset: reset})
	if err != nil || gemini.Calls != 2 || gemini.USD != 1.75 || gemini.Unpriced != 0 {
		t.Fatalf("gemini pool: %+v %v", gemini, err)
	}
	argon, err := reportQuotaWindow(i.Dir, QuotaWindow{Provider: "google", Model: pointer("argon"), Start: &start, Reset: reset})
	if err != nil || argon.Calls != 1 || argon.USD != 0.50 {
		t.Fatalf("argon pool: %+v %v", argon, err)
	}
	thirdParty, err := reportQuotaWindow(i.Dir, QuotaWindow{Provider: "google", Model: pointer("claude,gpt"), Start: &start, Reset: reset})
	if err != nil || thirdParty.Calls != 2 || thirdParty.USD != 2.00 || thirdParty.Unpriced != 1 {
		t.Fatalf("3p pool: %+v %v", thirdParty, err)
	}
}
