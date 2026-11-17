package collectors

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"quota-hub/internal/model"
)

// OpenCode Go uses only the read-only allowance endpoint. No free-model
// counters, billing requests, passive refusals, token refresh or inference.
// Origins are constants, never supplied by a credential.
type openCodeConsoleCollector struct {
	base
	oc      *openCodeKey
	goUsage string
}

type consoleCredential struct {
	token string
	orgID string
}

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
		if entry.IntegrationID == c.spec.IntegrationID && entry.Active {
			match = append(match, i)
		}
	}
	if len(match) != 1 {
		return consoleCredential{}, fail(CodeCredentialsUnavailable)
	}
	entry := wrapper.Data[match[0]]
	var token string
	switch entry.Value.Type {
	case "key":
		token = entry.Value.Key
	case "oauth":
		if entry.Value.Expires != nil && *entry.Value.Expires <= c.now().UnixMilli() {
			return consoleCredential{}, fail(CodeAuthRequired)
		}
		token = entry.Value.Access
	default:
		return consoleCredential{}, fail(CodeCredentialsUnavailable)
	}
	if !validHeaderToken(token) {
		return consoleCredential{}, fail(CodeCredentialsUnavailable)
	}
	return consoleCredential{token: token, orgID: entry.Value.Metadata.OrgID}, nil
}

func (c *openCodeConsoleCollector) Fetch(ctx context.Context) (model.Result, error) {
	ctx, cancel := c.context(ctx)
	defer cancel()
	cred, e := c.consoleCredential(ctx)
	if e != nil {
		return model.Result{}, e
	}
	headers := map[string]string{"Authorization": "Bearer " + cred.token}
	if cred.orgID != "" {
		headers["x-org-id"] = cred.orgID
		headers["x-opencode-org-id"] = cred.orgID
	}
	r, e := c.get(ctx, c.goUsage, headers, maxBodyBytes)
	if e != nil {
		return model.Result{}, e
	}
	if r.status != http.StatusOK {
		return model.Result{}, c.statusError(r)
	}
	observed := c.now().UTC()
	quotas, err := parseConsoleGo(r.body, observed)
	if err != nil {
		return model.Result{}, err
	}
	res, err := finish(observed, nil, quotas, nil, nil)
	res.Source = SourceOpenCodeConsole
	return res, err
}

// parseConsoleGo requires all three meters; missing/invalid data is never zero.
func parseConsoleGo(body []byte, observed time.Time) ([]model.Quota, error) {
	top := asObject(body)
	if top == nil {
		return nil, fail(CodeInvalidResponse)
	}
	usage := asObject(top["usage"])
	if usage == nil {
		return nil, fail(CodeInvalidResponse)
	}
	meters := []struct {
		id, field, label, period string
		duration                 int64
	}{
		{"go_rolling", "rolling", "5 часов", "", 18000},
		{"go_weekly", "weekly", "Неделя", "week", 604800},
		{"go_monthly", "monthly", "Месяц", "month", 0},
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
		limited, label := true, m.label
		quotas = append(quotas, model.Quota{
			ID: m.id, Metric: "quota", Unit: "percent",
			UsedPercent: fptr(round6(percent)), RemainingPercent: fptr(round6(100 - percent)),
			Window: w, Authority: authority, Label: &label, Limited: &limited, ObservedAt: tptr(observed),
		})
	}
	return quotas, nil
}

// windowSet merges native and passive Claude windows in stable remote order.
type windowSet struct {
	mu               sync.Mutex
	remote, observed []model.Quota
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

func quotaNewer(a, b model.Quota) bool {
	if a.ObservedAt == nil {
		return false
	}
	if b.ObservedAt == nil {
		return true
	}
	return a.ObservedAt.After(*b.ObservedAt)
}
