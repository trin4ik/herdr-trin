package collectors

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"quota-hub/internal/model"
)

const (
	// SourceClaudeCodeOAuth labels results acquired through the native Claude Code OAuth usage GET.
	SourceClaudeCodeOAuth = "claude_code_oauth"

	EndpointClaudeUsage = "https://api.anthropic.com/api/oauth/usage"

	claudeOAuthBeta = "oauth-2025-04-20"
	// ~/.claude.json carries project history and can be large; it is only scanned for one field.
	maxAccountFileBytes = 16 << 20
)

// claudeHybrid combines the passive OpenCode bridge file (Observe, network-free) with a
// single read-only GET using the native Claude Code access token (Fetch). It never reads
// the refresh token, never refreshes and never writes any file. Scheduling and freshness
// decisions belong to the engine.
type claudeHybrid struct {
	base
	passive  *claudeCollector
	endpoint string
	// accountPin is the lowercase hex sha256 of the expected oauthAccount.accountUuid; empty disables the pin.
	accountPin string
	// windows keeps the windows of both sources: the passive headers know the
	// session and weekly windows, the native usage endpoint knows every window
	// of the account, including the model-scoped ones.
	windows windowSet
}

func newClaudeHybrid(spec Spec, b base, endpoint string) (*claudeHybrid, error) {
	if spec.ObservedFile == "" || spec.CredentialFile == "" {
		return nil, fail(CodeConfigInvalid)
	}
	pin := strings.ToLower(strings.TrimSpace(spec.ClaudeExpectedAccountSHA256))
	if pin != "" {
		raw, err := hex.DecodeString(pin)
		if err != nil || len(raw) != sha256.Size || spec.ClaudeAccountFile == "" {
			return nil, fail(CodeConfigInvalid)
		}
	}
	return &claudeHybrid{base: b, passive: &claudeCollector{base: b}, endpoint: endpoint, accountPin: pin}, nil
}

// Observe reads the passive bridge file only; it makes no network request.
func (c *claudeHybrid) Observe(ctx context.Context) (model.Result, error) {
	res, err := c.passive.Fetch(ctx)
	if err != nil {
		return model.Result{}, err
	}
	res.Source = SourceOpenCodeObserved
	c.windows.setObserved(res.Quotas)
	res.Quotas = c.windows.all()
	return res, nil
}

// Fetch performs exactly one GET to the Claude OAuth usage endpoint.
func (c *claudeHybrid) Fetch(parent context.Context) (model.Result, error) {
	if err := parent.Err(); err != nil {
		return model.Result{}, classifyTransport(parent, err)
	}
	if e := c.checkAccount(); e != nil {
		return model.Result{}, e
	}
	token, e := c.accessToken()
	if e != nil {
		return model.Result{}, e
	}
	ctx, cancel := c.context(parent)
	defer cancel()
	r, e := c.get(ctx, c.endpoint, map[string]string{
		"Authorization":  "Bearer " + token,
		"anthropic-beta": claudeOAuthBeta,
	}, maxBodyBytes)
	if e != nil {
		return model.Result{}, e
	}
	if r.status != http.StatusOK {
		return model.Result{}, c.statusError(r)
	}
	res, err := parseClaudeUsage(r.body, c.now())
	if err != nil {
		return model.Result{}, err
	}
	res.Source = SourceClaudeCodeOAuth
	c.windows.setRemote(res.Quotas)
	res.Quotas = c.windows.all()
	// Attach the pinned local snapshot to a native usage response too. The engine
	// also synchronizes it independently at startup/scans, without waiting for idle.
	if p, metadataErr := c.localProfile(token); metadataErr == nil {
		res.Profile = p
		res.Plan = p.Plan
	}
	return res, nil
}

func (c *claudeHybrid) MetadataIdentity() (string, error) {
	if c.accountPin == "" {
		return "", fail(CodeCredentialsUnavailable)
	}
	data, ok := readBounded(c.spec.ClaudeAccountFile, maxAccountFileBytes)
	if !ok {
		return "", fail(CodeCredentialsUnavailable)
	}
	defer zero(data)
	var f struct {
		Account struct {
			UUID string `json:"accountUuid"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &f) != nil || f.Account.UUID == "" {
		return "", fail(CodeCredentialsUnavailable)
	}
	token, e := c.accessToken()
	if e != nil {
		return "", e
	}
	// Report the actual local identity even if it differs from the pin. This
	// invalidates cached metadata; checkAccount still blocks remote access.
	return "claude:" + identity(f.Account.UUID) + "." + identity(token), nil
}

// ReadLocalProfile does not wait for the idle OAuth fallback. SubscriptionType
// and pinned account metadata are local even while quota headers stay active.
func (c *claudeHybrid) ReadLocalProfile(ctx context.Context) (*model.Profile, error) {
	if err := ctx.Err(); err != nil {
		return nil, classifyTransport(ctx, err)
	}
	token, e := c.accessToken()
	if e != nil {
		return nil, e
	}
	return c.localProfile(token)
}

func (c *claudeHybrid) localProfile(expectedToken string) (*model.Profile, error) {
	if e := c.checkAccount(); e != nil {
		return nil, e
	}
	id, err := c.MetadataIdentity()
	if err != nil {
		return nil, err
	}
	data, ok := readBounded(c.spec.ClaudeAccountFile, maxAccountFileBytes)
	if !ok {
		return nil, fail(CodeCredentialsUnavailable)
	}
	defer zero(data)
	var account struct {
		OAuthAccount struct {
			UUID  string          `json:"accountUuid"`
			Email json.RawMessage `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &account) != nil || identity(account.OAuthAccount.UUID) != c.accountPin {
		return nil, fail(CodeCredentialsUnavailable)
	}
	creds, ok := readBounded(c.spec.CredentialFile, maxFileBytes)
	if !ok {
		return nil, fail(CodeCredentialsUnavailable)
	}
	defer zero(creds)
	var native struct {
		OAuth struct {
			Token string          `json:"accessToken"`
			Plan  json.RawMessage `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(creds, &native) != nil || native.OAuth.Token != expectedToken {
		return nil, fail(CodeCredentialsUnavailable)
	}
	var plan *string
	if p, ok := asString(native.OAuth.Plan); ok && planID.MatchString(p) {
		plan = sptr(p)
	}
	if current, err := c.MetadataIdentity(); err != nil || current != id {
		return nil, fail(CodeCredentialsUnavailable)
	}
	email, _ := asString(account.OAuthAccount.Email)
	return &model.Profile{Identity: id, AccountEmail: model.Email(email), Plan: plan}, nil
}

// checkAccount compares the hash of oauthAccount.accountUuid with the configured pin.
// Missing or mismatched account data uses the fixed credentials_unavailable category:
// nothing from the file (email, UUID, paths) is ever returned.
func (c *claudeHybrid) checkAccount() *model.ErrorInfo {
	if c.accountPin == "" {
		return nil
	}
	data, ok := readBounded(c.spec.ClaudeAccountFile, maxAccountFileBytes)
	if !ok {
		return fail(CodeCredentialsUnavailable)
	}
	var f struct {
		OAuthAccount struct {
			AccountUUID string `json:"accountUuid"`
		} `json:"oauthAccount"`
	}
	err := json.Unmarshal(data, &f)
	zero(data)
	uuid := f.OAuthAccount.AccountUUID
	if err != nil || uuid == "" {
		return fail(CodeCredentialsUnavailable)
	}
	sum := sha256.Sum256([]byte(uuid))
	got := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(c.accountPin)) != 1 {
		return fail(CodeCredentialsUnavailable)
	}
	return nil
}

// accessToken reads only claudeAiOauth.accessToken and expiresAt (milliseconds).
// A clearly expired token is rejected locally; the refresh token is never decoded.
func (c *claudeHybrid) accessToken() (string, *model.ErrorInfo) {
	data, ok := readBounded(c.spec.CredentialFile, maxFileBytes)
	if !ok {
		return "", fail(CodeCredentialsUnavailable)
	}
	var f struct {
		OAuth struct {
			AccessToken string          `json:"accessToken"`
			ExpiresAt   json.RawMessage `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	err := json.Unmarshal(data, &f)
	zero(data)
	token := f.OAuth.AccessToken
	if err != nil || !validHeaderToken(token) {
		return "", fail(CodeCredentialsUnavailable)
	}
	if len(f.OAuth.ExpiresAt) > 0 && string(f.OAuth.ExpiresAt) != "null" {
		n, ok := asNumber(f.OAuth.ExpiresAt)
		if !ok {
			return "", fail(CodeCredentialsUnavailable)
		}
		if n <= 0 || n > float64(time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()) || math.Trunc(n) != n {
			return "", fail(CodeCredentialsUnavailable)
		}
		exp := time.UnixMilli(int64(n)).UTC()
		if !c.now().Before(exp) {
			return "", fail(CodeAuthRequired)
		}
	}
	return token, nil
}

// parseClaudeUsage reads the usage endpoint. The modern limits[] array is the
// primary source: it carries every window the account has, including the
// model-scoped ones. The flat five_hour/seven_day keys are the fallback for an
// endpoint that does not report limits[], and are merged only where limits[] does
// not already describe the same window, so nothing is counted twice.
func parseClaudeUsage(body []byte, observed time.Time) (model.Result, error) {
	top := asObject(body)
	if top == nil {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	quotas, scoped := parseClaudeLimits(top, observed)
	flat := parseClaudeFlat(top, observed)
	for _, q := range flat {
		if scoped && strings.HasPrefix(q.ID, "seven_day_") {
			// The scoped pools are reported by limits[] only; the legacy flat
			// pool keys would be the same windows under another name.
			continue
		}
		duplicate := false
		for _, existing := range quotas {
			if existing.ID == q.ID {
				duplicate = true
				break
			}
		}
		if !duplicate {
			quotas = append(quotas, q)
		}
	}
	return finish(observed, nil, quotas, nil, body)
}

// claudeLimitEntry is one window of the limits[] array.
type claudeLimitEntry struct {
	Kind     string   `json:"kind"`
	Group    string   `json:"group"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			ID          *string `json:"id"`
			DisplayName *string `json:"display_name"`
		} `json:"model"`
		Surface *string `json:"surface"`
	} `json:"scope"`
}

// parseClaudeLimits reads the extensible limits[] array. An unknown kind is kept
// as its own window with a neutral label instead of being dropped, so a limit
// Anthropic adds later shows up without a code change.
func parseClaudeLimits(top map[string]json.RawMessage, observed time.Time) ([]model.Quota, bool) {
	raw, ok := top["limits"]
	if !ok {
		return nil, false
	}
	var entries []claudeLimitEntry
	if json.Unmarshal(raw, &entries) != nil || len(entries) == 0 {
		return nil, false
	}
	var quotas []model.Quota
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.Percent == nil {
			continue
		}
		used := *entry.Percent
		if used < 0 || used > 100 {
			continue
		}
		id, scope, label, w := claudeLimitWindow(entry)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if entry.ResetsAt != nil {
			if t, ok := parseTimeString(*entry.ResetsAt); ok {
				w.ResetAt = tptr(t)
			}
		}
		q := percentQuota(id, scope, used, w)
		q.Label = &label
		q.ObservedAt = tptr(observed)
		quotas = append(quotas, q)
	}
	return quotas, len(quotas) > 0
}

// claudeLimitWindow maps one limits[] entry to an id, a scope, a label and a
// window. The ids match the passive header ids (five_hour, seven_day), so both
// sources describe the same window and can be merged.
func claudeLimitWindow(entry claudeLimitEntry) (string, *string, string, model.Window) {
	w := unknownWindow()
	kind := strings.ToLower(strings.TrimSpace(entry.Kind))
	group := strings.ToLower(strings.TrimSpace(entry.Group))
	switch kind {
	case "session":
		w.DurationSeconds = i64ptr(18000)
		return "five_hour", nil, "Сессия", w
	case "weekly_all":
		w.Period = sptr("week")
		w.DurationSeconds = i64ptr(604800)
		return "seven_day", nil, "Неделя", w
	case "weekly_scoped":
		w.Period = sptr("week")
		w.DurationSeconds = i64ptr(604800)
		name, id := claudeScopedModel(entry)
		if name == "" {
			return "", nil, "", w
		}
		label := "Неделя"
		return "seven_day_" + slug(id), sptr(name), label, w
	}
	// An unknown kind keeps its own window: the group still describes its length.
	switch group {
	case "session":
		w.DurationSeconds = i64ptr(18000)
	case "weekly", "week":
		w.Period = sptr("week")
		w.DurationSeconds = i64ptr(604800)
	case "daily", "day":
		w.Period = sptr("day")
		w.DurationSeconds = i64ptr(86400)
	case "monthly", "month":
		w.Period = sptr("month")
	}
	id := slug(kind)
	if id == "" {
		return "", nil, "", w
	}
	if group != "" && group != kind {
		id += "_" + slug(group)
	}
	label := kind
	if label == "" {
		label = "Лимит"
	}
	return id, nil, label, w
}

// claudeScopedModel returns the display name and the slug of the model a scoped
// limit belongs to.
func claudeScopedModel(entry claudeLimitEntry) (string, string) {
	if entry.Scope == nil || entry.Scope.Model == nil {
		return "", ""
	}
	if entry.Scope.Model.ID != nil && strings.TrimSpace(*entry.Scope.Model.ID) != "" {
		name := *entry.Scope.Model.DisplayName
		if strings.TrimSpace(name) == "" {
			name = *entry.Scope.Model.ID
		}
		return name, *entry.Scope.Model.ID
	}
	if entry.Scope.Model.DisplayName != nil && strings.TrimSpace(*entry.Scope.Model.DisplayName) != "" {
		return *entry.Scope.Model.DisplayName, *entry.Scope.Model.DisplayName
	}
	return "", ""
}

// parseClaudeFlat reads the legacy flat keys (five_hour, seven_day and their
// pool suffixes).
func parseClaudeFlat(top map[string]json.RawMessage, observed time.Time) []model.Quota {
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]bool{}
	var quotas []model.Quota
	for _, k := range keys {
		token, ok := claudeUsageToken(k)
		if !ok {
			continue
		}
		o := asObject(top[k])
		if o == nil {
			continue
		}
		used, ok := asNumber(o["utilization"])
		if !ok || used < 0 {
			continue
		}
		id, scope, win := claudeWindow(token)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if t, ok := asTime(o["resets_at"]); ok {
			win.ResetAt = tptr(t)
		}
		q := percentQuota(id, scope, used, win)
		q.ObservedAt = tptr(observed)
		quotas = append(quotas, q)
	}
	return quotas
}

// claudeUsageToken maps a flat key to the header-style token understood by claudeWindow.
func claudeUsageToken(key string) (string, bool) {
	for _, p := range [][2]string{{"five_hour", "5h"}, {"seven_day", "7d"}} {
		if key == p[0] {
			return p[1], true
		}
		if strings.HasPrefix(key, p[0]+"_") {
			suffix := key[len(p[0])+1:]
			if suffix == "" || len(suffix) > 64 || strings.Trim(suffix, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
				return "", false
			}
			return p[1] + key[len(p[0]):], true
		}
	}
	return "", false
}
