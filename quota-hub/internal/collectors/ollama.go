package collectors

import (
	"context"
	"net/http"
	"time"

	"quota-hub/internal/model"
)

type ollamaCollector struct {
	base
	endpoint        string
	profileEndpoint string
	oc              *openCodeKey
}

// FetchProfile reads optional account metadata with the same key as usage.
func (c *ollamaCollector) FetchProfile(ctx context.Context, expected string) (model.ProfileUpdate, error) {
	return fetchKeyedProfile(ctx, &c.base, c.oc, expected, http.MethodPost, c.profileEndpoint, parseOllamaProfile)
}

func (c *ollamaCollector) Fetch(ctx context.Context) (model.Result, error) {
	return fetchKeyed(ctx, &c.base, c.oc, c.endpoint, func(body []byte) (model.Result, error) {
		return parseOllama(body, c.now())
	})
}

func parseOllama(body []byte, observed time.Time) (model.Result, error) {
	top := asObject(body)
	if top == nil {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	inc := asObject(top["included"])
	var quotas []model.Quota
	var metrics []model.Metric

	// Legacy plan: session/weekly percent windows. Session duration is not reported, so it stays null.
	for _, name := range []string{"session", "weekly"} {
		o := asObject(inc[name])
		if o == nil {
			continue
		}
		var used float64
		if rem, ok := asNumber(o["remaining_percent"]); ok {
			used = 100 - rem
		} else if u, ok := asNumber(o["used_percent"]); ok {
			used = u
		} else {
			continue
		}
		w := unknownWindow()
		if t, ok := asTime(o["resets_at"]); ok {
			w.ResetAt = tptr(t)
		}
		if name == "weekly" {
			w.Period = sptr("week")
		}
		quotas = append(quotas, percentQuota(name, nil, used, w))
	}

	// Credit plan: percent of the allowance still available; USD amounts are not mixed into the percent quota.
	allowance, okA := asNumber(inc["allowance_usd"])
	balance, okB := asNumber(inc["balance_usd"])
	if okA && okB && allowance > 0 && balance >= 0 {
		w := unknownWindow()
		id := "credit_allowance"
		period := asObject(inc["period"])
		if period == nil {
			period = asObject(top["period"])
		}
		from, okF := asTime(period["from"])
		until, okU := asTime(period["until"])
		if okU {
			w.ResetAt = tptr(until)
		}
		if okF && okU && until.After(from) {
			w.StartedAt = tptr(from)
			w.DurationSeconds = i64ptr(int64(until.Sub(from) / time.Second))
			if from.AddDate(0, 1, 0).Equal(until) {
				w.Kind = "calendar"
				w.Period = sptr("month")
				id = "monthly"
			}
		}
		quotas = append(quotas, percentQuota(id, nil, 100-balance/allowance*100, w))
	}

	if pur := asObject(top["purchased"]); pur != nil {
		if v, ok := asNumber(pur["balance_usd"]); ok && v >= 0 {
			metrics = append(metrics, model.Metric{ID: "purchased_balance", Metric: "balance", Unit: "USD", Value: v, Authority: authority})
		}
	}
	return finish(observed, nil, quotas, metrics, body)
}
