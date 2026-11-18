package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quota-hub/internal/model"
)

func TestUsageSnapshot(t *testing.T) {
	now := t0
	_, s, dir := setup(t, &now)
	reset := now.Add(time.Hour)
	if err := s.Commit(model.Update{ID: "claude", AttemptAt: now, NextAttemptAt: reset,
		Result: &model.Result{ObservedAt: now, Quotas: []model.Quota{
			{ID: "5h", Window: model.Window{ResetAt: &reset}},
		}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "summary.json")
	body := func(offset time.Duration, subscription, quota string) string {
		return `{"generatedAt":"` + now.Add(-time.Hour).Format(time.RFC3339) + `","current":[{
			"subscription":"` + subscription + `","quota":"` + quota + `","reset":"` + reset.Add(offset).Format(time.RFC3339) + `",
			"start":null,"partial":true,"calls":807,"usd":48.15,"unpriced":2,
			"tokens":{"input":100,"output":20,"reasoning":5,"cacheRead":30,"cacheWrite":10}}]}`
	}
	for _, tc := range []struct {
		name, content string
		enabled, want bool
	}{
		{"exact", body(0, "claude", "5h"), true, true},
		{"plus boundary", body(120*time.Second, "claude", "5h"), true, true},
		{"minus boundary", body(-120*time.Second, "claude", "5h"), true, true},
		{"plus mismatch", body(121*time.Second, "claude", "5h"), true, false},
		{"minus mismatch", body(-121*time.Second, "claude", "5h"), true, false},
		{"wrong subscription", body(0, "other", "5h"), true, false},
		{"wrong quota", body(0, "claude", "other"), true, false},
		{"disabled", body(0, "claude", "5h"), false, false},
		{"missing", "", true, false},
		{"broken", "{broken", true, false},
		{"bad date", `{"generatedAt":"bad","current":[]}`, true, false},
		{"oversize", strings.Repeat(" ", maxUsageSize+1), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.content == "" {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			configured := ""
			if tc.enabled {
				configured = path
			}
			handler := NewUnauthenticated(s, func() time.Time { return now }, configured)
			for range 2 { // Exercise both file reading and cached responses.
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/quotas", nil))
				if recorder.Code != http.StatusOK {
					t.Fatal(recorder.Code, recorder.Body.String())
				}
				var snapshot model.Snapshot
				if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
					t.Fatal(err)
				}
				var got *model.Usage
				for _, sub := range snapshot.Subscriptions {
					if sub.ID == "claude" {
						got = sub.Quotas[0].Usage
					}
				}
				if (got != nil) != tc.want {
					t.Fatalf("usage = %+v, want present %v", got, tc.want)
				}
				if got != nil && (got.Calls != 807 || got.CostUSD != 48.15 || got.Unpriced != 2 ||
					!got.Partial || got.Since != nil || got.InputTokens != 100 || got.OutputTokens != 20 ||
					got.ReasoningTokens != 5 || got.CacheReadTokens != 30 || got.CacheWriteTokens != 10 ||
					!got.GeneratedAt.Equal(now.Add(-time.Hour))) {
					t.Fatalf("wrong usage: %+v", got)
				}
			}
			if s.Snapshot(now).Subscriptions[0].Quotas[0].Usage != nil {
				t.Fatal("HTTP usage polluted store")
			}
		})
	}
}

func TestUsageFileCacheInvalidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.json")
	write := func(body string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	good := `{"generatedAt":"2026-11-18T12:00:00Z","current":[]}`
	write(good, t0)
	f := newUsageFile([]string{path})
	if !f.load() {
		t.Fatal("valid file rejected")
	}
	// Size changes invalidate even with an identical mtime; errors never use last-good.
	write("{", t0)
	if f.load() || f.summary != nil {
		t.Fatal("invalid file retained cached usage")
	}
	write(good, t0)
	if !f.load() {
		t.Fatal("valid replacement rejected")
	}
	write(strings.Replace(good, "12:00", "13:00", 1), t0.Add(time.Second))
	if !f.load() || !strings.Contains(f.summary.GeneratedAt, "13:00") {
		t.Fatal("mtime change not read")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if f.load() || f.summary != nil {
		t.Fatal("missing file retained cached usage")
	}
}
