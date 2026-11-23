// Package collectors implements read-only quota sources. It never refreshes credentials.
package collectors

import (
	"context"
	"net/http"
	"time"

	"quota-hub/internal/model"
)

type Collector interface {
	Fetch(context.Context) (model.Result, error)
}

// ProfileIdentity is a network-free identity check. It never refreshes credentials.
// The scheduler uses it to hide cached personal metadata before serving a restored
// snapshot and on each worker scan, including while remote polling is cooling down.
type ProfileIdentity interface {
	MetadataIdentity() (string, error)
}

// LocalProfileCollector reads pinned local metadata independently of passive
// observations and remote usage scheduling. It must never perform network I/O.
type LocalProfileCollector interface {
	ReadLocalProfile(context.Context) (*model.Profile, error)
}

// ProfileCollector is an optional, separately scheduled metadata read using the
// same source as usage. The implementation rechecks credentials after the HTTP
// response; the engine owns caching, deadlines and durable cooldown.
type ProfileCollector interface {
	FetchProfile(context.Context, string) (model.ProfileUpdate, error)
}

// HybridCollector observes a local source independently of remote GET scheduling.
// Observe is always network-free; Fetch performs exactly one remote quota GET.
type HybridCollector interface {
	Collector
	Observe(context.Context) (model.Result, error)
}

type Spec struct {
	model.Definition
	Interval          time.Duration
	Timeout           time.Duration
	Scope             string
	CredentialFile    string
	IntegrationID     string
	CredentialID      string
	OpenCodeURL       string
	OpenCodeBearerEnv string
	// OpenCodeServiceFile is an absolute path to the OpenCode local-service
	// registration (service.json). It excludes OpenCodeURL and OpenCodeBearerEnv.
	OpenCodeServiceFile string
	ObservedFile        string
	BackoffBase         time.Duration
	BackoffMax          time.Duration
	RateLimitPause      time.Duration
	AuthRetry           time.Duration
	// ClaudeNative enables the native Claude Code OAuth usage request. It runs on
	// the subscription's own interval and reports every window of the account,
	// including the model-scoped ones the passive headers never carry.
	ClaudeNative                bool
	ObserveInterval             time.Duration
	ClaudeAccountFile           string
	ClaudeExpectedAccountSHA256 string
	ClaudeProxyURL              string
	ClaudeProxyTokenEnv         string
	// ClaudeProfile reads e-mail and plan of a passive subscription from the
	// local Claude Code files; the passive collector stays network-free.
	ClaudeProfile bool
	// UsageProvidersOverride names the OpenCode providers whose calls consume a
	// Claude subscription.
	UsageProvidersOverride []string
}

// Options provides test seams. Production configuration cannot override upstream endpoints.
type Options struct {
	HTTPClient *http.Client
	Endpoints  map[string]string
	Now        func() time.Time
}

// ClaudeUsageProviders are the OpenCode providers that use a Claude subscription
// directly: the Anthropic API provider and the Claude Code harness. Each prices its
// own calls; the subscription shows their total.
var ClaudeUsageProviders = []string{"anthropic", "claude-agent"}

// UsageProviders lists every OpenCode provider whose calls consume this subscription.
func (s Spec) UsageProviders() []string {
	if len(s.UsageProvidersOverride) > 0 {
		return append([]string(nil), s.UsageProvidersOverride...)
	}
	if s.Provider == ProviderClaude && s.Source != SourceClaudeProxy {
		return append([]string(nil), ClaudeUsageProviders...)
	}
	if p := s.UsageProvider(); p != nil {
		return []string{*p}
	}
	return nil
}

// UsageProvider identifies the primary OpenCode provider of this subscription.
func (s Spec) UsageProvider() *string {
	if len(s.UsageProvidersOverride) > 0 {
		provider := s.UsageProvidersOverride[0]
		return &provider
	}
	var provider string
	switch {
	case s.Provider == ProviderClaude && s.Source == SourceClaudeProxy:
		provider = s.IntegrationID
	case s.Provider == ProviderCodex && s.Source == SourceNativeCodexOAuth:
		provider = "openai"
	case s.Provider == ProviderClaude:
		provider = "anthropic"
	case s.Source == SourceOpenCodeKey:
		provider = s.IntegrationID
	case s.Provider == ProviderOpenCode && s.Source == SourceOpenCodeConsole:
		provider = "opencode"
	}
	if provider == "" {
		return nil
	}
	return &provider
}
