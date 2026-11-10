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
// The passive half records a generic provider refusal observed by the OpenCode
// response hook. A 429 and Retry-After cannot identify a free-model allowance.

// consoleCredential is the bearer material of one Console workspace.
type consoleCredential struct {
	token string
	orgID string
	kind  string
}

type openCodeConsoleCollector struct {
	base
	oc         *openCodeKey
	server     string
	goUsage    string
	mu         sync.Mutex
	remote     []model.Quota
	metrics    []model.Metric
	remoteAt   time.Time
	observed   []model.Quota
	observedAt time.Time
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
	metrics, err := parseConsoleBilling(billing)
	if err != nil {
		return model.Result{}, err
	}

	var quotas []model.Quota
	models, _, e := c.consoleGet(ctx, c.server+"/api/usage/models?pageSize=100", cred.token, cred.orgID, "")
	if e != nil {
		return model.Result{}, e
	}
	modelQuotas, err := parseConsoleModels(models)
	if err != nil {
		return model.Result{}, err
	}
	quotas = append(quotas, modelQuotas...)

	// Budgets are optional: a workspace without policies answers with an empty list.
	budgets, status, e := c.consoleGet(ctx, c.server+"/api/policies/current-budgets", cred.token, cred.orgID, "")
	if e != nil {
		if status != http.StatusNotFound {
			return model.Result{}, e
		}
		budgets = nil
	}
	if status == http.StatusOK {
		budgetQuotas, err := parseConsoleBudgets(budgets)
		if err != nil {
			return model.Result{}, err
		}
		quotas = append(quotas, budgetQuotas...)
	}

	// The Go allowance lives on the inference origin and is refused (403) when the
	// workspace has no Go subscription.
	goBody, status, e := c.consoleGet(ctx, c.goUsage, cred.token, cred.orgID, "x-opencode-org-id")
	if e != nil {
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return model.Result{}, e
		}
	} else {
		goQuotas, err := parseConsoleGo(goBody, observed)
		if err != nil {
			return model.Result{}, err
		}
		quotas = append(quotas, goQuotas...)
	}

	for i := range quotas {
		quotas[i].ObservedAt = tptr(observed)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Publish/cache only a fully validated remote snapshot. Failed requests must
	// not partially replace the balance or windows preserved for passive reads.
	res, err := finish(observed, nil, append(append([]model.Quota{}, quotas...), c.activeRefusals(c.now())...), metrics, nil)
	if err != nil {
		return model.Result{}, err
	}
	if c.remoteAt.IsZero() || !observed.Before(c.remoteAt) {
		c.remote, c.metrics, c.remoteAt = quotas, metrics, observed
	}
	res.Source = SourceOpenCodeConsole
	return res, nil
}

// Observe reads the passive provider refusal. It never makes a request.
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
	found := false
	for _, q := range obs.Quota {
		if (q.Name != "rate_limit_reset" && q.Name != "free_limit_reset") || q.Type != "time" {
			continue
		}
		reset, ok := asTime(q.Value)
		found = true
		if !ok || !reset.After(at) {
			return model.Result{}, fail(CodeInvalidResponse)
		}
		if !c.now().Before(reset) {
			continue // historical refusal, not evidence of either denial or recovery now
		}
		w := unknownWindow()
		w.ResetAt = tptr(reset)
		label := "Отказ провайдера"
		quotas = append(quotas, model.Quota{
			ID: "provider_refusal", Metric: "counter", Unit: "status",
			Window: w, Authority: authority, ObservedAt: tptr(at.UTC()), Label: &label,
		})
	}
	if !found || len(quotas) > 1 {
		return model.Result{}, fail(CodeInvalidResponse)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !at.Before(c.observedAt) {
		c.observed, c.observedAt = quotas, at
	}
	if c.remoteAt.IsZero() {
		return model.Result{}, fail(CodeSourceUnavailable)
	}
	// Metrics have no individual timestamp in schema v1. Retain the remote
	// timestamp, rather than falsely refreshing old money with a local refusal.
	res, err := finish(c.remoteAt, nil, append(append([]model.Quota{}, c.remote...), c.activeRefusals(c.now())...), c.metrics, nil)
	if err != nil {
		return model.Result{}, err
	}
	res.Source = SourceOpenCodeObserved
	return res, nil
}

// activeRefusals is called with mu held. Expiry removes an active refusal; it
// never asserts that a renewed allowance is available.
func (c *openCodeConsoleCollector) activeRefusals(now time.Time) []model.Quota {
	var active []model.Quota
	for _, q := range c.observed {
		if q.Window.ResetAt != nil && now.Before(*q.Window.ResetAt) {
			active = append(active, q)
		}
	}
	return active
}

// parseConsoleBilling reads the prepaid balance of the workspace.
func parseConsoleBilling(body []byte) ([]model.Metric, error) {
	top := asObject(body)
	if top == nil {
		return nil, fail(CodeInvalidResponse)
	}
	var metrics []model.Metric
	micro, ok := asNumber(top["balanceMicroCents"])
	if !ok || micro < 0 {
		return nil, fail(CodeInvalidResponse)
	}
	metrics = append(metrics, model.Metric{ID: "credits_balance", Metric: "balance", Unit: "USD",
		Value: round6(micro / 1e6), Authority: authority})
	if raw, exists := top["creditLimitMicroCents"]; exists {
		micro, ok := asNumber(raw)
		if !ok || micro < 0 {
			return nil, fail(CodeInvalidResponse)
		}
		if micro > 0 {
			metrics = append(metrics, model.Metric{ID: "credit_limit", Metric: "balance", Unit: "USD",
				Value: round6(micro / 1e6), Authority: authority})
		}
	}
	return metrics, nil
}

// parseConsoleModels reads the per-model usage counters. The Console reports the
// totals it has seen for the workspace, free models included.
func parseConsoleModels(body []byte) ([]model.Quota, error) {
	top := asObject(body)
	if top == nil {
		return nil, fail(CodeInvalidResponse)
	}
	var items []json.RawMessage
	if json.Unmarshal(top["items"], &items) != nil || items == nil {
		return nil, fail(CodeInvalidResponse)
	}
	// Until the pagination request contract is confirmed, refuse a truncated
	// snapshot rather than report partial counters as complete workspace usage.
	pageInfo := asObject(top["pageInfo"])
	if pageInfo == nil {
		return nil, fail(CodeInvalidResponse)
	}
	page, okP := asNumber(pageInfo["page"])
	size, okS := asNumber(pageInfo["pageSize"])
	total, okT := asNumber(pageInfo["total"])
	count, okC := asNumber(pageInfo["pageCount"])
	if !okP || !okS || !okT || !okC || page != 1 || size != 100 || total != float64(len(items)) ||
		len(items) > 100 || count < 0 || count > 1 || (len(items) > 0 && count != 1) {
		return nil, fail(CodeInvalidResponse)
	}
	var quotas []model.Quota
	ids := map[string]bool{}
	for _, raw := range items {
		o := asObject(raw)
		if o == nil {
			return nil, fail(CodeInvalidResponse)
		}
		name, ok := asString(o["model"])
		if !ok || strings.TrimSpace(name) == "" || slug(name) == "" || ids[slug(name)] {
			return nil, fail(CodeInvalidResponse)
		}
		ids[slug(name)] = true
		requests, ok := asNumber(o["totalRequests"])
		if !ok || requests < 0 {
			return nil, fail(CodeInvalidResponse)
		}
		counters := map[string]float64{}
		for key, field := range map[string]string{
			"input_tokens":      "totalInputTokens",
			"output_tokens":     "totalOutputTokens",
			"cache_read_tokens": "totalCacheReadTokens",
		} {
			if raw, exists := o[field]; exists {
				v, ok := asNumber(raw)
				if !ok || v < 0 {
					return nil, fail(CodeInvalidResponse)
				}
				counters[key] = v
			}
		}
		if raw, exists := o["totalCostMicroCents"]; exists {
			v, ok := asNumber(raw)
			if !ok || v < 0 {
				return nil, fail(CodeInvalidResponse)
			}
			counters["cost_usd"] = round6(v / 1e6)
		}
		// Subscription billing is not evidence that inference was free. Keep the
		// provider's model name, but do not infer a free-model pool from this flag.
		if raw, exists := o["subscription"]; exists {
			if string(raw) == "null" {
				return nil, fail(CodeInvalidResponse)
			}
			_, ok := asBool(raw)
			if !ok {
				return nil, fail(CodeInvalidResponse)
			}
		}
		label := name
		quotas = append(quotas, counterQuota("model_"+slug(name), nil, &label, requests, "requests", counters, unknownWindow()))
	}
	return quotas, nil
}

// parseConsoleBudgets reads the workspace budgets: a spent share of a limit.
func parseConsoleBudgets(body []byte) ([]model.Quota, error) {
	var list []json.RawMessage
	if json.Unmarshal(body, &list) != nil || list == nil {
		return nil, fail(CodeInvalidResponse)
	}
	var quotas []model.Quota
	for _, raw := range list {
		o := asObject(raw)
		if o == nil {
			return nil, fail(CodeInvalidResponse)
		}
		rule, ok := asString(o["ruleID"])
		if !ok || rule == "" {
			return nil, fail(CodeInvalidResponse)
		}
		limit, okL := asNumber(o["limitDollars"])
		spent, okS := asNumber(o["spentDollars"])
		if !okL || !okS || limit <= 0 || spent < 0 {
			return nil, fail(CodeInvalidResponse)
		}
		w := unknownWindow()
		if raw, exists := o["resetsAt"]; exists {
			t, ok := asTime(raw)
			if !ok {
				return nil, fail(CodeInvalidResponse)
			}
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
	return quotas, nil
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
func parseConsoleGo(body []byte, observed time.Time) ([]model.Quota, error) {
	top := asObject(body)
	if top == nil {
		return nil, fail(CodeInvalidResponse)
	}
	usage := asObject(top["usage"])
	if usage == nil {
		return nil, fail(CodeInvalidResponse)
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
			return nil, fail(CodeInvalidResponse)
		}
		percent, ok := asNumber(o["percent"])
		if !ok || percent < 0 || percent > 100 {
			return nil, fail(CodeInvalidResponse)
		}
		w := unknownWindow()
		if m.period != "" {
			w.Period = sptr(m.period)
		}
		if m.duration > 0 {
			w.DurationSeconds = i64ptr(m.duration)
		}
		if raw, exists := o["resetsAt"]; exists {
			t, ok := asTime(raw)
			if !ok {
				return nil, fail(CodeInvalidResponse)
			}
			w.ResetAt = tptr(t)
		}
		if raw, exists := o["status"]; exists {
			status, ok := asString(raw)
			if !ok || (status != "ok" && status != "rate-limited") {
				return nil, fail(CodeInvalidResponse)
			}
		}
		// A finite allowance remains capped even while requests are allowed.
		limited := true
		label := m.label
		quotas = append(quotas, model.Quota{
			ID: m.id, Metric: "quota", Unit: "percent",
			UsedPercent: fptr(round6(percent)), RemainingPercent: fptr(round6(100 - percent)),
			Window: w, Authority: authority, Label: &label, Limited: &limited, ObservedAt: tptr(observed),
		})
	}
	return quotas, nil
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
