package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func valid(t *testing.T) Config {
	t.Helper()
	return Config{
		Listen: "192.168.50.10:8098", DataDir: filepath.Join(t.TempDir(), "state"),
		HTTPTokenEnv: "QUOTA_HUB_HTTP_TOKEN", OpenCode: OpenCode{URL: "http://192.168.50.10:49374"},
		Subscriptions: []Subscription{
			{ID: "codex-primary", Provider: "codex", Enabled: true, CredentialFile: "/nonexistent/native/auth.json"},
			{ID: "ollama-primary", Provider: "ollama", Enabled: true},
			{ID: "kimi-primary", Provider: "kimi", Enabled: true, IntegrationID: "kimi-code-plan-cn"},
			{ID: "claude-primary", Provider: "claude", Enabled: true, ObservedFile: "/nonexistent/claude.latest.json"},
			{ID: "opencode-go", Provider: "opencode_go", Enabled: false},
		},
	}
}

func TestSpecsSeparateProvidersFromCredentialSources(t *testing.T) {
	c := valid(t)
	ss, err := c.Specs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 4 {
		t.Fatalf("disabled provider leaked: %d", len(ss))
	}
	wantSources := []string{"native_codex_oauth", "opencode_key", "opencode_key", "opencode_observed"}
	for i, s := range ss {
		if s.Source != wantSources[i] || s.Timeout != 15*time.Second {
			t.Fatalf("bad source/default: %+v", s.Definition)
		}
		if i != 3 && s.Interval != 5*time.Minute {
			t.Fatal("polling default too aggressive")
		}
	}
	if ss[3].Interval != 30*time.Second || ss[3].CredentialFile != "" {
		t.Fatal("passive adapter must only check a local file")
	}
	if ss[1].IntegrationID != "ollama-cloud" || ss[2].IntegrationID != "kimi-code-plan-cn" {
		t.Fatal("wrong credential selection")
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	cases := map[string]func(*Config){
		"wildcard":     func(c *Config) { c.Listen = "0.0.0.0:8098" },
		"public":       func(c *Config) { c.Listen = "8.8.8.8:8098" },
		"missing_port": func(c *Config) { c.Listen = "192.168.50.10" },
		"bad_port":     func(c *Config) { c.Listen = "192.168.50.10:65536" },
		"relative_dir": func(c *Config) { c.DataDir = "data/service" },
		"root_dir":     func(c *Config) { c.DataDir = "/" },
		"empty_token":  func(c *Config) { c.HTTPTokenEnv = "" },
		"token_value":  func(c *Config) { c.HTTPTokenEnv = "Bearer secret" },
		"bad_id":       func(c *Config) { c.Subscriptions[0].ID = "../../elsewhere" },
		"duplicate_id": func(c *Config) { c.Subscriptions[1].ID = c.Subscriptions[0].ID },
		"duplicate_source": func(c *Config) {
			c.Subscriptions = append(c.Subscriptions, Subscription{ID: "ollama-copy", Provider: "ollama", Enabled: true})
		},
		"provider": func(c *Config) { c.Subscriptions[0].Provider = "unsupported" },
		"fallback": func(c *Config) { c.Subscriptions[0].Source = "opencode_key" },
		"relative_credential": func(c *Config) {
			c.Subscriptions[0].CredentialFile = "auth.json"
		},
		"claude_credentials":   func(c *Config) { c.Subscriptions[3].CredentialFile = "/secret/auth.json" },
		"relative_observation": func(c *Config) { c.Subscriptions[3].ObservedFile = "claude.latest.json" },
		"missing_region":       func(c *Config) { c.Subscriptions[2].IntegrationID = "" },
		"wrong_region":         func(c *Config) { c.Subscriptions[2].IntegrationID = "moonshotai" },
		"wrong_ollama":         func(c *Config) { c.Subscriptions[1].IntegrationID = "openai" },
		"fast_poll":            func(c *Config) { c.Subscriptions[0].Interval = "5s" },
		"fast_local_check":     func(c *Config) { c.Subscriptions[3].Interval = "1s" },
		"bad_stale":            func(c *Config) { c.Subscriptions[0].StaleAfter = "never" },
		"huge_timeout":         func(c *Config) { c.Subscriptions[0].Timeout = "24h" },
		"zero_interval":        func(c *Config) { c.Subscriptions[0].Interval = "0s" },
		"no_upstream":          func(c *Config) { c.OpenCode.URL = "" },
		"public_upstream":      func(c *Config) { c.OpenCode.URL = "https://example.com" },
		"url_credentials":      func(c *Config) { c.OpenCode.URL = "http://user:password@192.168.50.10:1234" },
		"url_query":            func(c *Config) { c.OpenCode.URL = "http://192.168.50.10:1234?token=secret" },
		"url_path":             func(c *Config) { c.OpenCode.URL = "http://192.168.50.10:1234/api/credential" },
		"unsafe_scope":         func(c *Config) { c.Subscriptions[0].Scope = "Bearer token" },
		"relative_service": func(c *Config) {
			c.OpenCode = OpenCode{ServiceFile: "opencode/service.json"}
		},
		"service_and_url": func(c *Config) {
			c.OpenCode.ServiceFile = "/nonexistent/opencode/service.json"
		},
		"service_and_token": func(c *Config) {
			c.OpenCode = OpenCode{ServiceFile: "/nonexistent/opencode/service.json", TokenEnv: "OPENCODE_TOKEN"}
		},
		"none_enabled": func(c *Config) {
			for i := range c.Subscriptions {
				c.Subscriptions[i].Enabled = false
			}
		},
		"fast_429": func(c *Config) { c.Subscriptions[0].Retry.RateLimitPause = "1s" },
		"bad_backoff": func(c *Config) {
			c.Subscriptions[0].Retry.BackoffBase = "1h"
			c.Subscriptions[0].Retry.BackoffMax = "5m"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid(t)
			mutate(&c)
			if _, err := c.Specs(); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestLoadStrictAndNoCredentialsRead(t *testing.T) {
	c := valid(t)
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("validation tried to read absent credentials or state: %v", err)
	}
	if _, err := os.Stat(c.DataDir); !os.IsNotExist(err) {
		t.Fatal("validation created runtime state")
	}
	for _, text := range []string{
		`{"api_key":"DO_NOT_ECHO_SECRET"}`, "[]", "null", "{} {}",
		strings.Repeat("x", maxConfigSize+1),
	} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil || strings.Contains(err.Error(), "DO_NOT_ECHO_SECRET") {
			t.Fatal("invalid config accepted or secret echoed")
		}
	}
}

func TestExampleIsValid(t *testing.T) {
	c, err := Load("../../configs/example.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.OpenCode.ServiceFile == "" || c.OpenCode.URL != "" || c.OpenCode.TokenEnv != "" {
		t.Fatal("example must use the service registration only, with no inline URL or token")
	}
}

func TestServiceFileIsValidatedWithoutReading(t *testing.T) {
	dir := t.TempDir()
	registration := filepath.Join(dir, "missing", "service.json")
	c := valid(t)
	c.OpenCode = OpenCode{ServiceFile: registration}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("absent registration must not fail validation: %v", err)
	}
	ss, err := loaded.Specs()
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range ss {
		if s.OpenCodeServiceFile != registration || s.OpenCodeURL != "" || s.OpenCodeBearerEnv != "" {
			t.Fatalf("spec %d: wrong OpenCode source fields", i)
		}
	}
	if _, err := os.Stat(filepath.Dir(registration)); !os.IsNotExist(err) {
		t.Fatal("validation touched the registration location")
	}
}

func TestStaticURLAndBearerRemainSupported(t *testing.T) {
	c := valid(t)
	c.OpenCode.TokenEnv = "OPENCODE_TOKEN"
	ss, err := c.Specs()
	if err != nil {
		t.Fatal(err)
	}
	if ss[1].OpenCodeURL != "http://192.168.50.10:49374" || ss[1].OpenCodeBearerEnv != "OPENCODE_TOKEN" || ss[1].OpenCodeServiceFile != "" {
		t.Fatal("static URL + bearer env compatibility lost")
	}
}

func TestPerProviderRetryPolicies(t *testing.T) {
	c := valid(t)
	c.Subscriptions[1].Retry = Retry{BackoffBase: "2m", BackoffMax: "2h", RateLimitPause: "45m", AuthRetry: "3h"}
	ss, err := c.Specs()
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].RateLimitPause != 15*time.Minute || ss[1].RateLimitPause != 45*time.Minute ||
		ss[1].BackoffBase != 2*time.Minute || ss[1].BackoffMax != 2*time.Hour || ss[1].AuthRetry != 3*time.Hour {
		t.Fatal("individual retry policy was not retained")
	}
}

func TestUnauthenticatedHTTPRequiresExplicitOptOut(t *testing.T) {
	c := valid(t)
	c.HTTPTokenEnv = ""
	if _, err := c.Specs(); err == nil {
		t.Fatal("missing token silently enabled anonymous access")
	}
	c.HTTPAuth = "none"
	if _, err := c.Specs(); err != nil {
		t.Fatal(err)
	}
	c.HTTPTokenEnv = "LEFTOVER_TOKEN"
	if _, err := c.Specs(); err == nil {
		t.Fatal("contradictory token configuration accepted")
	}
	c.HTTPTokenEnv, c.HTTPAuth = "", "off"
	if _, err := c.Specs(); err == nil {
		t.Fatal("unknown auth mode accepted")
	}
}

func TestDockerProfileUsesReadOnlyNativeSourcesAndNoRemoteKeys(t *testing.T) {
	c, err := Load("../../configs/docker.json")
	if err != nil {
		t.Fatal(err)
	}
	specs, err := c.Specs()
	if err != nil || len(specs) != 2 || c.HTTPAuth != "none" || c.Listen != "0.0.0.0:8098" || !c.AllowWildcardListen || c.PublishAccountEmail {
		t.Fatal("invalid Docker profile")
	}
	if c.DataDir != "/var/lib/quota-hub" || c.OpenCode.ServiceFile != "" || c.OpenCode.URL != "" {
		t.Fatal("Docker profile must not inherit a host credential API")
	}
	if specs[0].Provider != "codex" || specs[0].CredentialFile != "/credentials/codex/auth.json" ||
		specs[1].Provider != "claude" || specs[1].CredentialFile != "/credentials/claude/.credentials.json" {
		t.Fatal("wrong native sources")
	}
}

func TestWildcardListenerRequiresExplicitContainerOptIn(t *testing.T) {
	c := valid(t)
	for _, host := range []string{"0.0.0.0:8098", "[::]:8098"} {
		c.Listen, c.AllowWildcardListen = host, false
		if _, err := c.Specs(); err == nil {
			t.Fatal("wildcard silently enabled")
		}
		c.AllowWildcardListen = true
		if _, err := c.Specs(); err != nil {
			t.Fatal(err)
		}
	}
	c.Listen = "8.8.8.8:8098"
	if _, err := c.Specs(); err == nil {
		t.Fatal("container opt-in allowed public IP")
	}
}

func TestClaudeHybridUsesNativeCredentialsAndSeparateIntervals(t *testing.T) {
	c := valid(t)
	c.Subscriptions[3].ClaudeFallback = &ClaudeFallback{Enabled: true, CredentialFile: "/not-opened/.credentials.json", IdleAfter: "10m",
		AccountFile: "/not-opened/.claude.json", ExpectedAccountSHA256: strings.Repeat("a", 64)}
	specs, err := c.Specs()
	if err != nil {
		t.Fatal(err)
	}
	s := specs[3]
	if s.Interval != 5*time.Minute || s.ObserveInterval != 30*time.Second || s.ClaudeIdleAfter != 10*time.Minute ||
		s.CredentialFile != "/not-opened/.credentials.json" || s.ClaudeExpectedAccountSHA256 != strings.Repeat("a", 64) {
		t.Fatal("hybrid did not keep passive activity distinct from native polling")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Subscriptions[3].ClaudeFallback.CredentialFile = "relative.json" },
		func(c *Config) { c.Subscriptions[3].ClaudeFallback.IdleAfter = "0s" },
		func(c *Config) { c.Subscriptions[3].ClaudeFallback.ExpectedAccountSHA256 = "not-a-hash" },
		func(c *Config) { c.Subscriptions[3].ObserveInterval = "1s" },
		func(c *Config) { c.Subscriptions[3].Interval = "30s" },
	} {
		base := valid(t)
		copy := *c.Subscriptions[3].ClaudeFallback
		base.Subscriptions[3].ClaudeFallback = &copy
		mutate(&base)
		if _, err := base.Specs(); err == nil {
			t.Fatal("unsafe hybrid configuration accepted")
		}
	}
}
