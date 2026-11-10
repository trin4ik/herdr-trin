// Package config loads a strict, secret-free JSON configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/model"
)

const maxConfigSize = 1 << 20

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
var accountFingerprint = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Config struct {
	Listen              string         `json:"listen"`
	AllowWildcardListen bool           `json:"allow_wildcard_listen,omitempty"`
	DataDir             string         `json:"data_dir"`
	HTTPAuth            string         `json:"http_auth,omitempty"`
	HTTPTokenEnv        string         `json:"http_token_env,omitempty"`
	PublishAccountEmail bool           `json:"publish_account_email,omitempty"`
	OpenCode            OpenCode       `json:"opencode"`
	Subscriptions       []Subscription `json:"subscriptions"`
}

// OpenCode selects how the local OpenCode credential API is reached: either
// ServiceFile (the local-service registration, which supplies URL and Basic
// password on every fetch) or URL with an optional TokenEnv bearer. The two
// modes are mutually exclusive; ServiceFile never combines with URL/TokenEnv.
type OpenCode struct {
	URL         string `json:"url,omitempty"`
	TokenEnv    string `json:"token_env,omitempty"`
	ServiceFile string `json:"service_file,omitempty"`
}

type Subscription struct {
	ID              string          `json:"id"`
	Provider        string          `json:"provider"`
	Enabled         bool            `json:"enabled"`
	Source          string          `json:"source,omitempty"`
	Interval        string          `json:"interval,omitempty"`
	StaleAfter      string          `json:"stale_after,omitempty"`
	Timeout         string          `json:"timeout,omitempty"`
	Scope           string          `json:"scope,omitempty"`
	CredentialFile  string          `json:"credential_file,omitempty"`
	IntegrationID   string          `json:"integration_id,omitempty"`
	CredentialID    string          `json:"credential_id,omitempty"`
	ObservedFile    string          `json:"observed_file,omitempty"`
	ObserveInterval string          `json:"observe_interval,omitempty"`
	ClaudeFallback  *ClaudeFallback `json:"claude_fallback,omitempty"`
	Retry           Retry           `json:"retry,omitempty"`
}

type ClaudeFallback struct {
	Enabled               bool   `json:"enabled"`
	CredentialFile        string `json:"credential_file,omitempty"`
	AccountFile           string `json:"account_file,omitempty"`
	ExpectedAccountSHA256 string `json:"expected_account_sha256,omitempty"`
}

type Retry struct {
	BackoffBase    string `json:"backoff_base,omitempty"`
	BackoffMax     string `json:"backoff_max,omitempty"`
	RateLimitPause string `json:"rate_limit_pause,omitempty"`
	AuthRetry      string `json:"auth_retry,omitempty"`
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("cannot open configuration file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxConfigSize {
		return Config{}, errors.New("configuration must be a regular file of at most 1 MiB")
	}
	d := json.NewDecoder(io.LimitReader(f, maxConfigSize+1))
	d.DisallowUnknownFields()
	var c Config
	if err := d.Decode(&c); err != nil {
		return Config{}, errors.New("invalid configuration JSON or unknown field")
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return Config{}, errors.New("configuration must contain exactly one JSON object")
	}
	if _, err := c.Specs(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Specs validates without opening credentials, resolving accounts or making requests.
func (c Config) Specs() ([]collectors.Spec, error) {
	host, port, err := net.SplitHostPort(c.Listen)
	ip, ipErr := netip.ParseAddr(host)
	n, portErr := strconv.Atoi(port)
	if err != nil || ipErr != nil || (ip.IsUnspecified() && !c.AllowWildcardListen) || portErr != nil || n < 1 || n > 65535 {
		return nil, errors.New("listen must be a specific IP address and port; wildcard listeners are disabled")
	}
	if !ip.IsPrivate() && !ip.IsLoopback() && !(ip.IsUnspecified() && c.AllowWildcardListen) {
		return nil, errors.New("listen must use a private LAN or loopback IP")
	}
	if !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) == string(filepath.Separator) {
		return nil, errors.New("data_dir must be an absolute non-root directory")
	}
	switch c.HTTPAuth {
	case "", "bearer":
		if !envName.MatchString(c.HTTPTokenEnv) {
			return nil, errors.New("http_token_env must name an environment variable, not contain a token")
		}
	case "none":
		if c.HTTPTokenEnv != "" {
			return nil, errors.New("http_auth none must not configure http_token_env")
		}
	default:
		return nil, errors.New("http_auth must be bearer or explicit none for a trusted network")
	}
	if c.OpenCode.TokenEnv != "" && !envName.MatchString(c.OpenCode.TokenEnv) {
		return nil, errors.New("opencode.token_env must name an environment variable")
	}
	if c.OpenCode.ServiceFile != "" {
		if !filepath.IsAbs(c.OpenCode.ServiceFile) || strings.ContainsRune(c.OpenCode.ServiceFile, 0) {
			return nil, errors.New("opencode.service_file must be an absolute path")
		}
		if c.OpenCode.URL != "" || c.OpenCode.TokenEnv != "" {
			return nil, errors.New("opencode.service_file cannot be combined with opencode.url or opencode.token_env")
		}
	}
	if len(c.Subscriptions) > 32 {
		return nil, errors.New("at most 32 configured subscriptions are supported")
	}
	seen := make(map[string]bool)
	scopes := make(map[string]string)
	var specs []collectors.Spec
	for _, entry := range c.Subscriptions {
		if !identifier.MatchString(entry.ID) || seen[entry.ID] {
			return nil, errors.New("subscription IDs must be unique lowercase safe identifiers")
		}
		seen[entry.ID] = true
		if !entry.Enabled {
			continue
		}
		spec, err := c.spec(entry)
		if err != nil {
			return nil, fmt.Errorf("subscription %s: %w", entry.ID, err)
		}
		// Duplicate collectors for the same upstream credential would multiply polling.
		identity := spec.Provider + "|" + spec.IntegrationID + "|" + spec.CredentialID + "|" + spec.CredentialFile + "|" + spec.ObservedFile
		if previous, ok := scopes[identity]; ok {
			return nil, fmt.Errorf("subscriptions %s and %s duplicate one credential source", previous, spec.ID)
		}
		scopes[identity] = spec.ID
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return nil, errors.New("at least one subscription must be enabled")
	}
	return specs, nil
}

func (c Config) spec(entry Subscription) (collectors.Spec, error) {
	s := collectors.Spec{Definition: model.Definition{ID: entry.ID, Provider: entry.Provider, PublishAccountEmail: c.PublishAccountEmail},
		CredentialFile: entry.CredentialFile, IntegrationID: entry.IntegrationID,
		CredentialID: entry.CredentialID, ObservedFile: entry.ObservedFile,
		OpenCodeURL: strings.TrimRight(c.OpenCode.URL, "/"), OpenCodeBearerEnv: c.OpenCode.TokenEnv,
		OpenCodeServiceFile: c.OpenCode.ServiceFile}
	interval, stale := 5*time.Minute, 30*time.Minute
	if entry.Provider != "claude" && entry.Provider != "opencode" && (entry.ClaudeFallback != nil || entry.ObserveInterval != "") {
		return s, errors.New("Claude fallback and observe_interval are only valid for claude and opencode")
	}
	switch entry.Provider {
	case "codex":
		s.Source, s.Product = "native_codex_oauth", "codex"
		if !filepath.IsAbs(s.CredentialFile) || s.IntegrationID != "" || s.ObservedFile != "" {
			return s, errors.New("codex requires an absolute credential_file and no OpenCode integration/observed source")
		}
	case "ollama", "kimi":
		s.Source = "opencode_key"
		if s.OpenCodeServiceFile == "" {
			if err := validateOpenCodeURL(s.OpenCodeURL); err != nil {
				return s, err
			}
		}
		if s.CredentialFile != "" || s.ObservedFile != "" {
			return s, errors.New("OpenCode keys must not use credential_file or observed_file")
		}
		if entry.Provider == "ollama" {
			s.Product = "ollama_cloud"
			if s.IntegrationID == "" {
				s.IntegrationID = "ollama-cloud"
			}
			if s.IntegrationID != "ollama-cloud" {
				return s, errors.New("ollama requires integration_id ollama-cloud")
			}
		} else {
			if s.IntegrationID != "kimi-code-plan-cn" && s.IntegrationID != "kimi-code-plan-global" {
				return s, errors.New("kimi requires an explicit CN or Global integration_id")
			}
			s.Product = "kimi_code_cn"
			if s.IntegrationID == "kimi-code-plan-global" {
				s.Product = "kimi_code_global"
			}
		}
	case "claude":
		s.Source, s.Product = "opencode_observed", "claude_subscription"
		interval = 30 * time.Second // Backward-compatible passive-only source.
		if !filepath.IsAbs(s.ObservedFile) || s.CredentialFile != "" || s.IntegrationID != "" {
			return s, errors.New("claude requires an absolute observed_file; native OAuth belongs in claude_fallback")
		}
		if fallback := entry.ClaudeFallback; fallback != nil && fallback.Enabled {
			if !filepath.IsAbs(fallback.CredentialFile) || strings.ContainsRune(fallback.CredentialFile, 0) {
				return s, errors.New("claude_fallback.credential_file must be an absolute native Claude Code credential path")
			}
			s.CredentialFile = fallback.CredentialFile
			s.ClaudeNative = true
			var err error
			s.ObserveInterval, err = duration(entry.ObserveInterval, 30*time.Second, 15*time.Second, time.Hour)
			if err != nil {
				return s, errors.New("observe_interval must be between 15s and 1h")
			}
			interval = 5 * time.Minute
			if fallback.AccountFile != "" || fallback.ExpectedAccountSHA256 != "" {
				if !filepath.IsAbs(fallback.AccountFile) || strings.ContainsRune(fallback.AccountFile, 0) ||
					!accountFingerprint.MatchString(fallback.ExpectedAccountSHA256) {
					return s, errors.New("Claude account pin requires an absolute account_file and lowercase SHA256 hash")
				}
				s.ClaudeAccountFile, s.ClaudeExpectedAccountSHA256 = fallback.AccountFile, fallback.ExpectedAccountSHA256
			}
		} else if entry.ObserveInterval != "" {
			return s, errors.New("observe_interval requires enabled claude_fallback; passive-only sources use interval")
		}
	case "opencode":
		// The OpenCode Console (pay-as-you-go). The credential is the Console
		// credential of the local OpenCode service, never a file of this config.
		s.Source, s.Product = "opencode_console", "opencode_console"
		if s.OpenCodeServiceFile == "" {
			if err := validateOpenCodeURL(s.OpenCodeURL); err != nil {
				return s, err
			}
		}
		if s.CredentialFile != "" {
			return s, errors.New("opencode console must not use credential_file")
		}
		if s.IntegrationID == "" {
			s.IntegrationID = collectors.OpenCodeConsoleIntegration
		}
		if s.IntegrationID != collectors.OpenCodeConsoleIntegration {
			return s, errors.New("opencode console requires integration_id opencode")
		}
		if s.ObservedFile != "" && !filepath.IsAbs(s.ObservedFile) {
			return s, errors.New("opencode observed_file must be an absolute path")
		}
		if entry.ObserveInterval != "" {
			var err error
			s.ObserveInterval, err = duration(entry.ObserveInterval, 30*time.Second, 15*time.Second, time.Hour)
			if err != nil {
				return s, errors.New("observe_interval must be between 15s and 1h")
			}
		}
	default:
		return s, errors.New("unsupported enabled provider")
	}
	if entry.Source != "" && entry.Source != s.Source {
		return s, errors.New("source does not match provider; no automatic credential fallback")
	}
	var err error
	s.Interval, err = duration(entry.Interval, interval, 15*time.Second, 24*time.Hour)
	if err != nil || ((entry.Provider != "claude" || s.ClaudeNative) && s.Interval < time.Minute) {
		return s, errors.New("interval must be 1m..24h for polling or 15s..24h for observed files")
	}
	s.StaleAfter, err = duration(entry.StaleAfter, stale, time.Minute, 7*24*time.Hour)
	if err != nil {
		return s, errors.New("stale_after must be between 1m and 168h")
	}
	s.Timeout, err = duration(entry.Timeout, 15*time.Second, time.Second, time.Minute)
	if err != nil {
		return s, errors.New("timeout must be between 1s and 1m")
	}
	s.BackoffBase, err = duration(entry.Retry.BackoffBase, time.Minute, time.Minute, 24*time.Hour)
	if err != nil {
		return s, errors.New("retry.backoff_base must be between 1m and 24h")
	}
	s.BackoffMax, err = duration(entry.Retry.BackoffMax, time.Hour, time.Minute, 7*24*time.Hour)
	if err != nil || s.BackoffMax < s.BackoffBase {
		return s, errors.New("retry.backoff_max must be >= backoff_base and <=168h")
	}
	s.RateLimitPause, err = duration(entry.Retry.RateLimitPause, 15*time.Minute, time.Minute, 7*24*time.Hour)
	if err != nil {
		return s, errors.New("retry.rate_limit_pause must be between 1m and 168h")
	}
	s.AuthRetry, err = duration(entry.Retry.AuthRetry, 30*time.Minute, time.Minute, 7*24*time.Hour)
	if err != nil {
		return s, errors.New("retry.auth_retry must be between 1m and 168h")
	}
	s.Scope = entry.Scope
	if s.Scope == "" {
		// Conservatively share cooldown between all selected accounts of a vendor.
		s.Scope = entry.Provider
	}
	if !identifier.MatchString(s.Scope) {
		return s, errors.New("scope must be a safe identifier, not an account token")
	}
	return s, nil
}

func validateOpenCodeURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("opencode.url must be a trusted HTTP(S) origin without credentials, path or query")
	}
	host := u.Hostname()
	ip, err := netip.ParseAddr(host)
	if err == nil {
		if !ip.IsPrivate() && !ip.IsLoopback() {
			return errors.New("opencode.url must address a trusted local service")
		}
	} else if host != "localhost" && !strings.HasSuffix(host, ".box") {
		return errors.New("opencode.url hostname must be localhost or an internal .box name")
	}
	return nil
}

func duration(value string, fallback, min, max time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < min || d > max {
		return 0, errors.New("invalid duration")
	}
	return d, nil
}
