package collectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"quota-hub/internal/model"
)

// Fixed error codes. ErrorInfo never carries upstream bodies, URLs, paths or tokens.
const (
	CodeConfigInvalid          = "config_invalid"
	CodeCredentialsUnavailable = "credentials_unavailable"
	CodeSourceUnavailable      = "source_unavailable"
	CodeAuthRequired           = "auth_required"
	CodeAccessDenied           = "access_denied"
	CodeRateLimited            = "rate_limited"
	CodeUpstreamError          = "upstream_error"
	CodeInvalidResponse        = "invalid_response"
	CodeTimeout                = "timeout"
	CodeNetworkError           = "network_error"
	CodeCanceled               = "canceled"
	CodeProfileIdentityChanged = "profile_identity_changed"
)

const (
	ProviderCodex    = "codex"
	ProviderOllama   = "ollama"
	ProviderKimi     = "kimi"
	ProviderClaude   = "claude"
	ProviderOpenCode = "opencode"

	SourceNativeCodexOAuth = "native_codex_oauth"
	SourceOpenCodeKey      = "opencode_key"
	SourceOpenCodeObserved = "opencode_observed"
	SourceOpenCodeConsole  = "opencode_console"

	EndpointCodex    = "https://chatgpt.com/backend-api/wham/usage"
	EndpointOllama   = "https://ollama.com/api/balance"
	EndpointKimiCN   = "https://api.kimi.com/coding/v1/usages"
	EndpointKimiGlob = "https://api.kimi.ai/coding/v1/usages"

	EndpointOllamaProfile   = "https://ollama.com/api/me"
	EndpointKimiCNProfile   = "https://api.kimi.com/coding/v1/me"
	EndpointKimiGlobProfile = "https://api.kimi.ai/coding/v1/me"

	// The OpenCode Console is the pay-as-you-go gateway of opencode.ai. The
	// console origin is a constant: it is never taken from a credential or a
	// response, so a credential can never redirect the quota requests.
	EndpointConsole   = "https://opencode.ai/console"
	EndpointConsoleGo = "https://opencode.ai/zen/go/v1/usage"

	OpenCodeConsoleIntegration = "opencode"

	KimiGlobalIntegration = "kimi-code-plan-global"

	maxBodyBytes = 256 << 10
	maxCredBytes = 1 << 20
	maxFileBytes = 1 << 20
	maxObsBytes  = 256 << 10
	defaultTO    = 15 * time.Second
	maxTO        = 60 * time.Second
	futureSkew   = 5 * time.Minute
	userAgent    = "quota-hub/0.1"
	authority    = "provider_reported"
)

func fail(code string) *model.ErrorInfo { return &model.ErrorInfo{Code: code} }

func failStatus(code string, status int) *model.ErrorInfo {
	return &model.ErrorInfo{Code: code, HTTPStatus: status}
}

// base holds shared HTTP behaviour: copied client without redirects/cookies, bounded time and bodies.
type base struct {
	spec    Spec
	client  *http.Client
	now     func() time.Time
	timeout time.Duration
}

func newBase(spec Spec, opts Options) base {
	var c http.Client
	if opts.HTTPClient != nil {
		c = *opts.HTTPClient
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.Jar = nil
	to := spec.Timeout
	if to <= 0 {
		to = defaultTO
	}
	if to > maxTO {
		to = maxTO
	}
	c.Timeout = to
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return base{spec: spec, client: &c, now: now, timeout: to}
}

func (b *base) context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, b.timeout)
}

func classifyTransport(ctx context.Context, err error) *model.ErrorInfo {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return fail(CodeTimeout)
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return fail(CodeCanceled)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fail(CodeTimeout)
	}
	return fail(CodeNetworkError)
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// get performs exactly one GET. Non-200 bodies are drained (bounded) and dropped.
func (b *base) get(ctx context.Context, target string, headers map[string]string, limit int64) (*response, *model.ErrorInfo) {
	return b.request(ctx, http.MethodGet, target, headers, limit)
}

// request performs exactly one body-less request with the same client limits as get.
func (b *base) request(ctx context.Context, method, target string, headers map[string]string, limit int64) (*response, *model.ErrorInfo) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, fail(CodeConfigInvalid)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, classifyTransport(ctx, err)
	}
	defer resp.Body.Close()
	out := &response{status: resp.StatusCode, header: resp.Header}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
		return out, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, classifyTransport(ctx, err)
	}
	if int64(len(body)) > limit {
		return nil, failStatus(CodeInvalidResponse, resp.StatusCode)
	}
	out.body = body
	return out, nil
}

// statusError maps a non-200 response to a fixed ErrorInfo.
func (b *base) statusError(r *response) *model.ErrorInfo {
	switch {
	case r.status == http.StatusUnauthorized:
		return failStatus(CodeAuthRequired, r.status)
	case r.status == http.StatusForbidden:
		return failStatus(CodeAccessDenied, r.status)
	case r.status == http.StatusTooManyRequests:
		e := failStatus(CodeRateLimited, r.status)
		e.RetryAt = parseRetryAfter(r.header.Get("Retry-After"), b.now())
		return e
	case r.status >= 500 && r.status <= 599:
		e := failStatus(CodeUpstreamError, r.status)
		if r.status == http.StatusServiceUnavailable {
			e.RetryAt = parseRetryAfter(r.header.Get("Retry-After"), b.now())
		}
		return e
	default:
		return failStatus(CodeInvalidResponse, r.status)
	}
}

// parseRetryAfter accepts delta-seconds or an HTTP-date; long waits are preserved, not capped.
func parseRetryAfter(v string, now time.Time) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if strings.Trim(v, "0123456789") == "" {
		// Use Unix arithmetic: time.Duration overflows for very long server waits.
		// Beyond JSON's representable year range, fail closed until year 9999.
		last := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n > uint64(last.Unix()-now.Unix()) {
			return &last
		}
		t := time.Unix(now.Unix()+int64(n), int64(now.Nanosecond())).UTC()
		return &t
	}
	if t, err := http.ParseTime(v); err == nil {
		t = t.UTC()
		if t.After(now) {
			return &t
		}
	}
	return nil
}

// httpEndpoint validates a configured/injected URL. Credentials in URLs are refused.
func httpEndpoint(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", false
	}
	return u.String(), true
}

func readBounded(path string, limit int64) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	// A credential source must be a regular file, never a blocking FIFO/helper.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, false
	}
	return data, true
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func identity(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validHeaderToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// --- tolerant JSON helpers ---

func asObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func asString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func asNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		s = strings.TrimSpace(s)
		if s == "" || strings.Trim(s, "0123456789+-.eE") != "" {
			return 0, false
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	}
	if string(raw) == "null" || json.Unmarshal(raw, &f) != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func asBool(raw json.RawMessage) (bool, bool) {
	var b bool
	if len(raw) == 0 || json.Unmarshal(raw, &b) != nil {
		return false, false
	}
	return b, true
}

func epochTime(v float64) (time.Time, bool) {
	if v <= 0 {
		return time.Time{}, false
	}
	if v > 1e11 {
		v /= 1000
	}
	sec := math.Floor(v)
	t := time.Unix(int64(sec), int64((v-sec)*1e9)).UTC()
	if t.Year() < 2000 || t.Year() > 2200 {
		return time.Time{}, false
	}
	return t, true
}

func parseTimeString(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// asTime reads RFC3339, a naive UTC date-time, or epoch seconds/milliseconds.
func asTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	if raw[0] == '"' {
		s, ok := asString(raw)
		if !ok {
			return time.Time{}, false
		}
		if t, ok := parseTimeString(s); ok {
			return t, true
		}
	}
	if n, ok := asNumber(raw); ok {
		return epochTime(n)
	}
	return time.Time{}, false
}

func round6(v float64) float64 {
	if math.Abs(v) > math.MaxFloat64/1e6 {
		return v
	}
	return math.Round(v*1e6) / 1e6
}

func fptr(v float64) *float64 { return &v }
func sptr(s string) *string   { return &s }
func tptr(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}
func i64ptr(v int64) *int64 { return &v }

// percentQuota builds a percent-only quota; absolute used/limit/remaining stay null.
func percentQuota(id string, scope *string, usedPct float64, w model.Window) model.Quota {
	// Preserve reported overage/credit states, rather than silently clamping them.
	used := round6(usedPct)
	return model.Quota{
		ID:               id,
		Scope:            scope,
		Metric:           "quota",
		Unit:             "percent",
		UsedPercent:      fptr(used),
		RemainingPercent: fptr(round6(100 - used)),
		Window:           w,
		Authority:        authority,
	}
}

func unknownWindow() model.Window { return model.Window{Kind: "unknown"} }

// counterQuota builds an absolute-value quota (requests, tokens, money) instead of a
// percent window. Counters carry no limit: the provider does not publish one.
func counterQuota(id string, scope *string, label *string, used float64, unit string, counters map[string]float64, w model.Window) model.Quota {
	return model.Quota{
		ID:        id,
		Scope:     scope,
		Metric:    "counter",
		Unit:      unit,
		Used:      fptr(round6(used)),
		Window:    w,
		Authority: authority,
		Label:     label,
		Counters:  counters,
	}
}

func slug(s string) string {
	var out []byte
	prevUnderscore := true
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out = append(out, byte(r))
			prevUnderscore = false
		} else if !prevUnderscore {
			out = append(out, '_')
			prevUnderscore = true
		}
	}
	return strings.Trim(string(out), "_")
}

// finish validates that a result has data and copies the raw payload.
func finish(observed time.Time, plan *string, quotas []model.Quota, metrics []model.Metric, raw []byte) (model.Result, error) {
	if len(quotas) == 0 && len(metrics) == 0 {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	for _, q := range quotas {
		for _, p := range []*float64{q.Used, q.Limit, q.Remaining, q.UsedPercent, q.RemainingPercent} {
			if p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0)) {
				return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
			}
		}
		for _, v := range q.Counters {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
			}
		}
	}
	for _, m := range metrics {
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
		}
	}
	r := model.Result{ObservedAt: observed.UTC(), Plan: plan, Quotas: quotas, Metrics: metrics}
	if raw != nil {
		r.Raw = append(json.RawMessage(nil), raw...)
	}
	return r, nil
}
