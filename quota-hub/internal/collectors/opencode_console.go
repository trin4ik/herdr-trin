package collectors

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"quota-hub/internal/model"
)

// The OpenCode Console collector reads the pay-as-you-go workspace of opencode.ai
// through the Console API. Its credential is the Console credential of the local
// OpenCode service: the service registration supplies the URL and the Basic
// password, the credential API of that service holds the session token or a
// personal API key. The console origin itself is a constant of this package, so a
// credential can never redirect a quota request.
//
// Nothing is refreshed here. An expired Console session is reported as
// auth_required and the operator signs in again on that machine.
//
// The passive half of the subscription is the free-model limit state, observed by
// the OpenCode response hook: the Console publishes no remaining counter for free
// models, it only refuses them with 429 and a reset time.

// consoleCredential is the bearer material of one Console workspace.
type consoleCredential struct {
	token string
	orgID string
	kind  string
}

type openCodeConsoleCollector struct {
	base
	oc      *openCodeKey
	server  string
	goUsage string
	windows windowSet
}

// consoleCredential reads the active Console credential of the local OpenCode
// service. Only the active credential of the configured integration is used; an
// ambiguous store is refused instead of guessed.
func (c *openCodeConsoleCollector) consoleCredential(ctx context.Context) (consoleCredential, *model.ErrorInfo) {
	target, auth, e := c.oc.registrationFor("/api/credential")
	if e != nil {
		return consoleCredential{}, e
	}
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	r, e := c.oc.get(ctx, target, headers, maxCredBytes)
	if e != nil {
		return consoleCredential{}, e
	}
	if r.status != http.StatusOK {
		return consoleCredential{}, c.oc.statusError(r)
	}
	defer zero(r.body)
	var wrapper struct {
		Data []struct {
			ID            string `json:"id"`
			IntegrationID string `json:"integrationID"`
			Active        bool   `json:"active"`
			Value         struct {
				Type     string `json:"type"`
				Key      string `json:"key"`
				Access   string `json:"access"`
				Expires  *int64 `json:"expires"`
				Metadata struct {
					OrgID string `json:"orgID"`
				} `json:"metadata"`
			} `json:"value"`
		} `json:"data"`
	}
	if json.Unmarshal(r.body, &wrapper) != nil {
		return consoleCredential{}, fail(CodeCredentialsUnavailable)
	}
	var match []int
	for i, entry := range wrapper.Data {
		if entry.IntegrationID != c.spec.IntegrationID || !entry.Active {
			continue
		}
		match = append(match, i)
	}
	if len(match) != 1 {
		return consoleCredential{}, fail(CodeCredentialsUnavailable)
	}
	entry := wrapper.Data[match[0]]
	switch entry.Value.Type {
	case "key":
		if !validHeaderToken(entry.Value.Key) {
			return consoleCredential{}, fail(CodeCredentialsUnavailable)
		}
		return consoleCredential{token: entry.Value.Key, orgID: entry.Value.Metadata.OrgID, kind: "key"}, nil
	case "oauth":
		if !validHeaderToken(entry.Value.Access) {
			return consoleCredential{}, fail(CodeCredentialsUnavailable)
		}
		// The session token is used as is. quota-hub never refreshes it.
		if entry.Value.Expires != nil && *entry.Value.Expires <= c.now().UnixMilli() {
			return consoleCredential{}, fail(CodeAuthRequired)
		}
		return consoleCredential{token: entry.Value.Access, orgID: entry.Value.Metadata.OrgID, kind: "oauth"}, nil
	default:
		return consoleCredential{}, fail(CodeCredentialsUnavailable)
	}
}

// consoleGet performs one read-only Console request and returns the body. A
// non-200 answer is returned as a status, not an error: 403 means the workspace has
// no access to that resource, which is a state and not a failure.
func (c *openCodeConsoleCollector) consoleGet(ctx context.Context, url, token, orgID, extraHeader string) ([]byte, int, *model.ErrorInfo) {
	headers := map[string]string{"Authorization": "Bearer " + token}
	if orgID != "" {
		headers["x-org-id"] = orgID
	}
	if extraHeader != "" && orgID != "" {
		headers[extraHeader] = orgID
	}
	r, e := c.get(ctx, url, headers, maxBodyBytes)
	if e != nil {
		return nil, 0, e
	}
	if r.status != http.StatusOK {
		return nil, r.status, c.statusError(r)
	}
	return r.body, r.status, nil
}

// Fetch reads the Console workspace: balance, per-model usage, budgets and the Go
// allowance meters. Every request is read-only and bounded.
func (c *openCodeConsoleCollector) Fetch(ctx context.Context) (model.Result, error) {
	cred, e := c.consoleCredential(ctx)
	if e != nil {
		return model.Result{}, e
	}
	ctx, cancel := c.context(ctx)
	defer cancel()

	observed := c.now().UTC()

	billing, status, e := c.consoleGet(ctx, c.server+"/api/billing/status", cred.token, cred.orgID, "")
	if e != nil {
		return model.Result{}, e
	}
	metrics := parseConsoleBilling(billing)

	var quotas []model.Quota
	models, _, e := c.consoleGet(ctx, c.server+"/api/usage/models?pageSize=100", cred.token, cred.orgID, "")
	if e != nil {
		return model.Result{}, e
	}
	quotas = append(quotas, parseConsoleModels(models)...)

	// Budgets are optional: a workspace without policies answers with an empty list.
	budgets, status, e := c.consoleGet(ctx, c.server+"/api/policies/current-budgets", cred.token, cred.orgID, "")
	if e != nil {
		if status != http.StatusNotFound {
			return model.Result{}, e
		}
		budgets = nil
	}
	quotas = append(quotas, parseConsoleBudgets(budgets)...)

	// The Go allowance lives on the inference origin and is refused (403) when the
	// workspace has no Go subscription.
	goBody, status, e := c.consoleGet(ctx, c.goUsage, cred.token, cred.orgID, "x-opencode-org-id")
	if e != nil {
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return model.Result{}, e
		}
	} else {
		quotas = append(quotas, parseConsoleGo(goBody, observed)...)
	}

	c.windows.setRemote(quotas)
	res, err := finish(observed, nil, c.windows.all(), metrics, nil)
	if err != nil {
		return model.Result{}, err
	}
	res.Source = SourceOpenCodeConsole
	return res, nil
}

// Observe reads the passive free-model limit state. It never makes a request.
func (c *openCodeConsoleCollector) Observe(ctx context.Context) (model.Result, error) {
	if c.spec.ObservedFile == "" {
		return model.Result{}, fail(CodeSourceUnavailable)
	}
	data, ok := readBounded(c.spec.ObservedFile, maxObsBytes)
	if !ok {
		return model.Result{}, fail(CodeSourceUnavailable)
	}
	var obs struct {
		SchemaVersion int    `json:"schema_version"`
		Source        string `json:"source"`
		ProviderID    string `json:"provider_id"`
		ObservedAt    string `json:"observed_at"`
		Status        int    `json:"status"`
		Quota         []struct {
			Name  string          `json:"name"`
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"quota"`
	}
	if json.Unmarshal(data, &obs) != nil || obs.SchemaVersion != 1 || obs.Source != "opencode-http-response-hook" ||
		obs.ProviderID != "opencode" || obs.Status != http.StatusTooManyRequests {
		return model.Result{}, fail(CodeInvalidResponse)
	}
	at, err := time.Parse(time.RFC3339Nano, obs.ObservedAt)
	if err != nil || at.After(c.now().Add(futureSkew)) {
		return model.Result{}, fail(CodeInvalidResponse)
	}
	var quotas []model.Quota
	for _, q := range obs.Quota {
		if q.Name != "free_limit_reset" || q.Type != "time" {
			continue
		}
		reset, ok := asTime(q.Value)
		if !ok {
			continue
		}
		w := unknownWindow()
		w.ResetAt = tptr(reset)
		label := "Бесплатные модели"
		quotas = append(quotas, model.Quota{
			ID: "free_models", Metric: "quota", Unit: "percent",
			UsedPercent: fptr(100), RemainingPercent: fptr(0),
			Window: w, Authority: authority, ObservedAt: tptr(at.UTC()), Label: &label,
		})
	}
	if len(quotas) == 0 {
		return model.Result{}, fail(CodeInvalidResponse)
	}
	c.windows.setObserved(quotas)
	res, err := finish(at.UTC(), nil, c.windows.all(), nil, nil)
	if err != nil {
		return model.Result{}, err
	}
	res.Source = SourceOpenCodeObserved
	return res, nil
}

// parseConsoleBilling reads the prepaid balance of the workspace.
func parseConsoleBilling(body []byte) []model.Metric {
	top := asObject(body)
	if top == nil {
		return nil
	}
	var metrics []model.Metric
	if micro, ok := asNumber(top["balanceMicroCents"]); ok && micro >= 0 {
		metrics = append(metrics, model.Metric{ID: "credits_balance", Metric: "balance", Unit: "USD",
			Value: round6(micro / 1e6), Authority: authority})
	}
	if micro, ok := asNumber(top["creditLimitMicroCents"]); ok && micro > 0 {
		metrics = append(metrics, model.Metric{ID: "credit_limit", Metric: "balance", Unit: "USD",
			Value: round6(micro / 1e6), Authority: authority})
	}
	return metrics
}

// parseConsoleModels reads the per-model usage counters. The Console reports the
// totals it has seen for the workspace, free models included.
func parseConsoleModels(body []byte) []model.Quota {
	top := asObject(body)
	if top == nil {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(top["items"], &items) != nil {
		return nil
	}
	var quotas []model.Quota
	for _, raw := range items {
		o := asObject(raw)
		if o == nil {
			continue
		}
		name, ok := asString(o["model"])
		if !ok || name == "" {
			continue
		}
		requests, ok := asNumber(o["totalRequests"])
		if !ok || requests < 0 {
			continue
		}
		counters := map[string]float64{}
		for key, field := range map[string]string{
			"input_tokens":      "totalInputTokens",
			"output_tokens":     "totalOutputTokens",
			"cache_read_tokens": "totalCacheReadTokens",
		} {
			if v, ok := asNumber(o[field]); ok && v >= 0 {
				counters[key] = v
			}
		}
		if v, ok := asNumber(o["totalCostMicroCents"]); ok && v >= 0 {
			counters["cost_usd"] = round6(v / 1e6)
		}
		var scope *string
		if sub, ok := asBool(o["subscription"]); ok && !sub {
			scope = sptr("free")
		}
		label := name
		quotas = append(quotas, counterQuota("model_"+slug(name), scope, &label, requests, "requests", counters, unknownWindow()))
	}
	return quotas
}

// parseConsoleBudgets reads the workspace budgets: a spent share of a limit.
func parseConsoleBudgets(body []byte) []model.Quota {
	var list []json.RawMessage
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	var quotas []model.Quota
	for _, raw := range list {
		o := asObject(raw)
		if o == nil {
			continue
		}
		rule, ok := asString(o["ruleID"])
		if !ok || rule == "" {
			continue
		}
		limit, okL := asNumber(o["limitDollars"])
		spent, okS := asNumber(o["spentDollars"])
		if !okL || !okS || limit <= 0 || spent < 0 {
			continue
		}
		w := unknownWindow()
		if t, ok := asTime(o["resetsAt"]); ok {
			w.ResetAt = tptr(t)
		}
		scope, _ := asString(o["scope"])
		per, _ := asString(o["per"])
		label := "Бюджет"
		if scope != "" && per != "" {
			label = "Бюджет · " + scope + " · " + per
		}
		used := round6(spent / limit * 100)
		quotas = append(quotas, model.Quota{
			ID: "budget_" + slug(rule), Metric: "quota", Unit: "percent",
			UsedPercent: fptr(used), RemainingPercent: fptr(round6(100 - used)),
			Window: w, Authority: authority, Label: &label,
			Counters: map[string]float64{"limit_usd": round6(limit), "spent_usd": round6(spent)},
		})
	}
	return quotas
}

// goMeter is one window of the Go allowance.
type goMeter struct {
	id       string
	field    string
	label    string
	period   string
	duration int64
}

// parseConsoleGo reads the Go allowance meters: rolling, weekly and monthly.
func parseConsoleGo(body []byte, observed time.Time) []model.Quota {
	top := asObject(body)
	if top == nil {
		return nil
	}
	usage := asObject(top["usage"])
	if usage == nil {
		return nil
	}
	meters := []goMeter{
		{id: "go_rolling", field: "rolling", label: "Go · 5 часов", duration: 18000},
		{id: "go_weekly", field: "weekly", label: "Go · Неделя", period: "week", duration: 604800},
		{id: "go_monthly", field: "monthly", label: "Go · Месяц", period: "month"},
	}
	var quotas []model.Quota
	for _, m := range meters {
		o := asObject(usage[m.field])
		if o == nil {
			continue
		}
		percent, ok := asNumber(o["percent"])
		if !ok || percent < 0 || percent > 100 {
			continue
		}
		w := unknownWindow()
		if m.period != "" {
			w.Period = sptr(m.period)
		}
		if m.duration > 0 {
			w.DurationSeconds = i64ptr(m.duration)
		}
		if t, ok := asTime(o["resetsAt"]); ok {
			w.ResetAt = tptr(t)
		}
		limited := false
		if status, ok := asString(o["status"]); ok {
			limited = strings.EqualFold(status, "rate-limited")
		}
		label := m.label
		quotas = append(quotas, model.Quota{
			ID: m.id, Metric: "quota", Unit: "percent",
			UsedPercent: fptr(round6(percent)), RemainingPercent: fptr(round6(100 - percent)),
			Window: w, Authority: authority, Label: &label, Limited: &limited, ObservedAt: tptr(observed),
		})
	}
	return quotas
}

// windowSet keeps the windows of one subscription per source and publishes their
// union in a stable order. A window only one source knows about is kept, so a
// limit that only the remote API reports does not disappear when a passive
// observation arrives.
type windowSet struct {
	mu       sync.Mutex
	remote   []model.Quota
	observed []model.Quota
}

func (w *windowSet) setRemote(quotas []model.Quota) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.remote = quotas
}

func (w *windowSet) setObserved(quotas []model.Quota) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.observed = quotas
}

// all merges both sources by id and keeps the order of the remote source, so
// the windows of a subscription do not jump around between refreshes. The newer
// value of an id wins.
func (w *windowSet) all() []model.Quota {
	w.mu.Lock()
	defer w.mu.Unlock()
	merged := make([]model.Quota, 0, len(w.remote)+len(w.observed))
	index := map[string]int{}
	put := func(q model.Quota) {
		if i, known := index[q.ID]; known {
			if !quotaNewer(merged[i], q) {
				merged[i] = q
			}
			return
		}
		index[q.ID] = len(merged)
		merged = append(merged, q)
	}
	for _, q := range w.remote {
		put(q)
	}
	for _, q := range w.observed {
		put(q)
	}
	return merged
}

// quotaNewer reports whether a is confirmed more recently than b.
func quotaNewer(a, b model.Quota) bool {
	if a.ObservedAt == nil {
		return false
	}
	if b.ObservedAt == nil {
		return true
	}
	return a.ObservedAt.After(*b.ObservedAt)
}
