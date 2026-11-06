package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"quota-hub/internal/config"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

func TestCheckConfigIsOffline(t *testing.T) {
	var out, errs bytes.Buffer
	code := run([]string{"-config", "../../configs/example.json", "-check-config"}, &out, &errs)
	if code != 0 {
		t.Fatalf("code=%d: %s", code, errs.String())
	}
	var result struct {
		Valid   bool     `json:"valid"`
		Enabled []string `json:"enabled_subscriptions"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Valid || len(result.Enabled) != 1 {
		t.Fatalf("unexpected check output: %s", out.String())
	}
}

func TestCLIRejectsUnsafeOrConflictingStartup(t *testing.T) {
	for _, args := range [][]string{
		{"-config", "../../configs/example.json", "-check-config", "-once"},
		{"extra-argument"},
		{"-config", "does-not-exist.json", "-check-config"},
	} {
		var out, errs bytes.Buffer
		if code := run(args, &out, &errs); code == 0 {
			t.Fatalf("accepted invalid arguments %v", args)
		}
	}
}

func TestMissingTokenStopsBeforeStorageOrCollection(t *testing.T) {
	t.Setenv("QUOTA_HUB_HTTP_TOKEN", "")
	var out, errs bytes.Buffer
	if code := run([]string{"-config", "../../configs/example.json"}, &out, &errs); code == 0 {
		t.Fatal("started server with no authentication token")
	}
	if !bytes.Contains(errs.Bytes(), []byte("bearer-token")) {
		t.Fatalf("unexpected failure path: %s", errs.String())
	}
}

func TestResumeAuthIsOfflineAndCannotClearRateLimits(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		t.Run(httpStatusName(status), func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.Config{Listen: "127.0.0.1:8098", DataDir: filepath.Join(dir, "data"), HTTPTokenEnv: "QUOTA_HUB_HTTP_TOKEN",
				Subscriptions: []config.Subscription{{ID: "codex", Provider: "codex", Enabled: true, CredentialFile: filepath.Join(dir, "never-opened-auth.json")}}}
			path := filepath.Join(dir, "config.json")
			body, _ := json.Marshal(cfg)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			specs, err := cfg.Specs()
			if err != nil {
				t.Fatal(err)
			}
			s, err := store.Open(cfg.DataDir, []model.Definition{specs[0].Definition})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			code := "auth_required"
			if status == 429 {
				code = "rate_limited"
			} else if status == 503 {
				code = "upstream_error"
			}
			deadline := now.Add(time.Hour)
			failure := &model.ErrorInfo{Code: code, HTTPStatus: status}
			if status == 429 {
				failure.RetryAt = &deadline
			}
			if err := s.Commit(model.Update{ID: "codex", AttemptAt: now, NextAttemptAt: deadline, Failure: failure}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			var out, errs bytes.Buffer
			exit := run([]string{"-config", path, "-resume-auth", "codex"}, &out, &errs)
			allowed := status == 401 || status == 403
			if (exit == 0) != allowed {
				t.Fatalf("unexpected auth-resume result for %d: exit=%d %s", status, exit, errs.String())
			}
			s, err = store.Open(cfg.DataDir, []model.Definition{specs[0].Definition})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			sub, _ := s.Get("codex")
			if allowed {
				if sub.Error.Code != "auth_source_reconfigured" || !sub.NextAttemptAt.Before(deadline) {
					t.Fatal("auth pause was not rescheduled")
				}
			} else if !sub.NextAttemptAt.Equal(deadline) {
				t.Fatal("non-auth/server cooldown was changed")
			}
			if _, err := os.Stat(specs[0].CredentialFile); !os.IsNotExist(err) {
				t.Fatal("offline operator action touched credentials")
			}
		})
	}
}

func httpStatusName(status int) string {
	switch status {
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 429:
		return "rate_limited"
	default:
		return "upstream_error"
	}
}

func TestResumeHybridAuthKeepsPassiveGoodAndNeverClearsRemote429(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(httpStatusName(status), func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.Config{Listen: "127.0.0.1:8098", DataDir: filepath.Join(dir, "data"), HTTPAuth: "none",
				Subscriptions: []config.Subscription{{ID: "claude", Provider: "claude", Enabled: true, ObservedFile: filepath.Join(dir, "never-opened-header.json"),
					ClaudeFallback: &config.ClaudeFallback{Enabled: true, CredentialFile: filepath.Join(dir, "never-opened-token.json")}}}}
			path := filepath.Join(dir, "config.json")
			body, _ := json.Marshal(cfg)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			specs, err := cfg.Specs()
			if err != nil {
				t.Fatal(err)
			}
			s, err := store.Open(cfg.DataDir, []model.Definition{specs[0].Definition})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			deadline := now.Add(time.Hour)
			failure := &model.ErrorInfo{Code: "auth_required", HTTPStatus: status}
			if status == 429 {
				failure.Code, failure.RetryAt = "rate_limited", &deadline
			}
			hs := model.HybridState{LastPassiveAt: &now, NextRemoteAttemptAt: &deadline, RemoteError: failure, RemoteFailures: 1}
			if err := s.Commit(model.Update{ID: "claude", AttemptAt: now, NextAttemptAt: now.Add(30 * time.Second),
				Result: &model.Result{ObservedAt: now, Source: "opencode_observed", Quotas: []model.Quota{}}, Hybrid: &hs}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			var out, errs bytes.Buffer
			exit := run([]string{"-config", path, "-resume-auth", "claude"}, &out, &errs)
			if (exit == 0) != (status == 401) {
				t.Fatalf("hybrid auth reschedule: status=%d exit=%d %s", status, exit, errs.String())
			}
			s, err = store.Open(cfg.DataDir, []model.Definition{specs[0].Definition})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			sub, _ := s.GetAt("claude", now)
			updated, _ := s.Hybrid("claude")
			if sub.Error != nil || sub.Stale || !sub.LastSuccessAt.Equal(now) {
				t.Fatal("operator reschedule spoiled fresh passive data")
			}
			if status == 429 && !updated.NextRemoteAttemptAt.Equal(deadline) {
				t.Fatal("hybrid server rate limit was cleared")
			}
		})
	}
}
