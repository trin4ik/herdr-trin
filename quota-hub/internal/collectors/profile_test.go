package collectors

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"quota-hub/internal/model"
)

func TestCodexProfileFields(t *testing.T) {
	for _, tc := range []struct {
		name                string
		email, plan         any
		wantEmail, wantPlan string
	}{
		{"known", "  person@example.test  ", "prolite", "person@example.test", "prolite"},
		{"null", nil, nil, "", ""},
		{"invalid types", 123, []string{"max"}, "", ""},
		{"not email", "organization-123", "pro", "", "pro"},
		{"long", strings.Repeat("a", 301) + "@example.test", strings.Repeat("p", 101), "", ""},
		{"controls", "person@example.test\n", "pro\t", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := asObject(fixture(t, "codex_usage.json"))
			body["email"], _ = json.Marshal(tc.email)
			body["plan_type"], _ = json.Marshal(tc.plan)
			b, _ := json.Marshal(body)
			r, err := parseCodex(b, fakeNow)
			if err != nil || r.Profile == nil {
				t.Fatalf("profile missing: %v", err)
			}
			if profileValue(r.Profile.AccountEmail) != tc.wantEmail || profileValue(r.Profile.Plan) != tc.wantPlan {
				t.Fatal("unexpected metadata")
			}
			if len(r.Quotas) != 1 {
				t.Fatal("metadata changed usage")
			}
		})
	}
}

func profileValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestKeyedProvidersDoNotInventProfiles(t *testing.T) {
	for _, tc := range []struct {
		name, fixture string
		parse         func([]byte, time.Time) (model.Result, error)
	}{
		{"ollama", "ollama_legacy.json", parseOllama}, {"kimi", "kimi_live.json", parseKimi},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range []any{nil, "person@example.test", 123, strings.Repeat("a", 400), "bad\nvalue"} {
				body := asObject(fixture(t, tc.fixture))
				body["email"], _ = json.Marshal(v)
				body["plan"], _ = json.Marshal(v)
				b, _ := json.Marshal(body)
				r, err := tc.parse(b, fakeNow)
				if err != nil || r.Profile != nil || r.Plan != nil || len(r.Quotas) == 0 {
					t.Fatal("unsupported top-level metadata must not be trusted")
				}
			}
		})
	}
}

func TestClaudePinnedLocalProfile(t *testing.T) {
	for _, tc := range []struct {
		name                string
		email, plan         any
		wantEmail, wantPlan string
	}{
		{"known", " person@example.test ", "max", "person@example.test", "max"},
		{"null", nil, nil, "", ""}, {"invalid", 123, []int{1}, "", ""},
		{"long", strings.Repeat("a", 301) + "@example.test", strings.Repeat("p", 101), "", ""},
		{"controls", "person@example.test\r", "max\n", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds := writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli())
			data, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": oauthTestToken, "subscriptionType": tc.plan}})
			if err := os.WriteFile(creds, data, 0600); err != nil {
				t.Fatal(err)
			}
			acc := writeAccountFile(t, oauthTestUUID)
			data, _ = json.Marshal(map[string]any{"oauthAccount": map[string]any{"accountUuid": oauthTestUUID, "emailAddress": tc.email}})
			if err := os.WriteFile(acc, data, 0600); err != nil {
				t.Fatal(err)
			}
			spec := hybridSpec(creds, "/not-read-by-fetch")
			spec.ClaudeAccountFile = acc
			spec.ClaudeExpectedAccountSHA256 = pinOf(oauthTestUUID)
			srv := serve(t, jsonHandler([]byte(oauthUsageBody)))
			h := hybridFetcher(t, spec, srv)
			r, err := h.Fetch(context.Background())
			if err != nil || r.Profile == nil {
				t.Fatalf("profile: %v", err)
			}
			if profileValue(r.Profile.AccountEmail) != tc.wantEmail || profileValue(r.Profile.Plan) != tc.wantPlan || r.Profile.Identity != "claude:"+pinOf(oauthTestUUID)+"."+identity(oauthTestToken) {
				t.Fatal("wrong profile")
			}
			if srv.hits.Load() != 1 {
				t.Fatal("metadata must not add requests")
			}
		})
	}
}

func TestClaudeMetadataFailureDoesNotDestroyUsage(t *testing.T) {
	creds := writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli())
	account := writeAccountFile(t, oauthTestUUID)
	spec := hybridSpec(creds, "/not-read")
	spec.ClaudeAccountFile = account
	spec.ClaudeExpectedAccountSHA256 = pinOf(oauthTestUUID)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// Failure after the pre-request account check, but before metadata parsing.
		if err := os.WriteFile(account, []byte(`{"oauthAccount":`), 0600); err != nil {
			t.Error(err)
		}
		jsonHandler([]byte(oauthUsageBody))(w, r)
	})
	h := hybridFetcher(t, spec, srv)
	r, err := h.Fetch(context.Background())
	if err != nil || r.Profile != nil || len(r.Quotas) == 0 {
		t.Fatalf("metadata failure damaged usage: %v", err)
	}
}

func TestCodexLateProfileBlockedOnAccountSwitch(t *testing.T) {
	file := writeAuth(t, "synthetic-token", "account-a")
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(file, []byte(`{"tokens":{"access_token":"other-synthetic","account_id":"account-b"}}`), 0600); err != nil {
			t.Error(err)
		}
		body := asObject(fixture(t, "codex_usage.json"))
		body["email"] = json.RawMessage(`"a@example.test"`)
		b, _ := json.Marshal(body)
		jsonHandler(b)(w, r)
	})
	c, err := New(codexSpec(file), opts(map[string]string{"codex": srv.URL}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Fetch(context.Background())
	if err != nil || r.Profile == nil || !r.Profile.Invalidated || len(r.Quotas) == 0 {
		t.Fatal("late metadata must be invalidated independently of usage")
	}
}
