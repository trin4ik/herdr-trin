// Package model defines the public, credential-free quota contract.
package model

import (
	"encoding/json"
	"time"
)

const SchemaVersion = 1

type Snapshot struct {
	SchemaVersion int            `json:"schema_version"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Subscriptions []Subscription `json:"subscriptions"`
}

type Subscription struct {
	ID            string     `json:"id"`
	Provider      string     `json:"provider"`
	Product       string     `json:"product"`
	Source        string     `json:"source"`
	Plan          *string    `json:"plan"`
	AccountEmail  *string    `json:"account_email"`
	Status        string     `json:"status"`
	Stale         bool       `json:"stale"`
	StaleAfter    *time.Time `json:"stale_after"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`
	LastPassiveAt *time.Time `json:"last_passive_at,omitempty"`
	NextPollAt    *time.Time `json:"next_poll_at,omitempty"`
	LastPollError *ErrorInfo `json:"last_poll_error,omitempty"`
	Error         *ErrorInfo `json:"error"`
	Quotas        []Quota    `json:"quotas"`
	Metrics       []Metric   `json:"metrics"`
}

type Quota struct {
	ID               string   `json:"id"`
	Scope            *string  `json:"scope"`
	Metric           string   `json:"metric"`
	Unit             string   `json:"unit"`
	Used             *float64 `json:"used"`
	Limit            *float64 `json:"limit"`
	Remaining        *float64 `json:"remaining"`
	UsedPercent      *float64 `json:"used_percent"`
	RemainingPercent *float64 `json:"remaining_percent"`
	Window           Window   `json:"window"`
	Authority        string   `json:"authority"`
	// Label is an optional provider-supplied name of the window (a model, a budget).
	Label *string `json:"label,omitempty"`
	// Limited is false when the provider reports the window as uncapped.
	Limited *bool `json:"limited,omitempty"`
	// ObservedAt is when this window's value was last confirmed by the provider.
	// It matters when windows of one subscription come from sources with different
	// cadences: a window may be older than the subscription itself.
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	// Counters carries extra absolute values of a counter metric (tokens, cost).
	// Only known keys are rendered; unknown keys are ignored by older clients.
	Counters map[string]float64 `json:"counters,omitempty"`
}

type Window struct {
	Kind            string     `json:"kind"`
	Period          *string    `json:"period"`
	DurationSeconds *int64     `json:"duration_seconds"`
	StartedAt       *time.Time `json:"started_at"`
	ResetAt         *time.Time `json:"reset_at"`
}

type Metric struct {
	ID        string  `json:"id"`
	Metric    string  `json:"metric"`
	Unit      string  `json:"unit"`
	Value     float64 `json:"value"`
	Authority string  `json:"authority"`
}

// ErrorInfo contains a fixed category, never a raw upstream body or credential path.
type ErrorInfo struct {
	Code                   string     `json:"code"`
	HTTPStatus             int        `json:"http_status,omitempty"`
	RetryAt                *time.Time `json:"retry_at,omitempty"`
	ProfileIdentity        string     `json:"-"`
	ProfileIdentityChecked bool       `json:"-"`
}

func (e *ErrorInfo) Error() string { return e.Code }

// Definition is non-secret public identity plus freshness policy.
type Definition struct {
	ID                  string
	Provider            string
	Product             string
	Source              string
	StaleAfter          time.Duration
	PublishAccountEmail bool // Explicit operator consent; false even with bearer by default.
}

// Result.Raw is PRIVATE and must never be marshalled into public HTTP/history output.
// For observed sources, ObservedAt is the response time, not the file-read time.
type Result struct {
	ObservedAt      time.Time
	Source          string `json:"-"` // Actual acquisition source when a collector is hybrid.
	Identity        string `json:"-"` // Private stable account/credential fingerprint, not a token.
	Plan            *string
	Profile         *Profile `json:"-"` // nil: not checked/error; non-nil: authoritative (nullable fields).
	ProfileIdentity string   `json:"-"` // Keyed source credential generation, not quota account identity.
	Quotas          []Quota
	Metrics         []Metric
	Raw             json.RawMessage `json:"-"`
}

// Profile is private, account-bound metadata, independent of usage freshness.
// Invalidated permanently hides metadata for this subscription ID after a switch.
type Profile struct {
	Identity     string  `json:"identity"`
	AccountEmail *string `json:"account_email"`
	Plan         *string `json:"plan"`
	Invalidated  bool    `json:"invalidated,omitempty"`
}

// ProfileState schedules optional remote metadata independently of usage. It is
// private, persisted, and never marshalled into the public subscription.
type ProfileState struct {
	Identity      string     `json:"identity"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	Failures      int        `json:"failures"`
	Error         *ErrorInfo `json:"error,omitempty"`
}

// ProfileUpdate records one metadata request, with a post-response credential
// check. A result for an obsolete ExpectedIdentity is never applied.
type ProfileUpdate struct {
	ExpectedIdentity string
	CurrentIdentity  string
	IdentityChecked  bool
	AttemptAt        time.Time
	NextAttemptAt    time.Time
	Profile          *Profile
	Failure          *ErrorInfo
}

// Update is one completed local attempt. Exactly one of Result or Failure is set.
type Update struct {
	ID            string
	AttemptAt     time.Time
	NextAttemptAt time.Time
	Result        *Result
	Failure       *ErrorInfo
	Hybrid        *HybridState
}

// HybridState is persisted separately from quota freshness. An API success must
// never make the bridge look active or erase a remote rate-limit deadline.
type HybridState struct {
	LastPassiveAt       *time.Time `json:"last_passive_at,omitempty"`
	LastRemoteAttemptAt *time.Time `json:"last_remote_attempt_at,omitempty"`
	NextRemoteAttemptAt *time.Time `json:"next_remote_attempt_at,omitempty"`
	RemoteFailures      int        `json:"remote_failures"`
	RemoteError         *ErrorInfo `json:"remote_error,omitempty"`
}
