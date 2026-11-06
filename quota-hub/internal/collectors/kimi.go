package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"quota-hub/internal/model"
)

type kimiCollector struct {
	base
	endpoint        string
	profileEndpoint string
	oc              *openCodeKey
}

// FetchProfile reads optional account metadata with the same key as usage.
func (c *kimiCollector) FetchProfile(ctx context.Context, expected string) (model.ProfileUpdate, error) {
	return fetchKeyedProfile(ctx, &c.base, c.oc, expected, http.MethodGet, c.profileEndpoint, parseKimiProfile)
}

func (c *kimiCollector) Fetch(ctx context.Context) (model.Result, error) {
	return fetchKeyed(ctx, &c.base, c.oc, c.endpoint, func(body []byte) (model.Result, error) {
		return parseKimi(body, c.now())
	})
}

func parseKimi(body []byte, observed time.Time) (model.Result, error) {
	top := asObject(body)
	if top == nil {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	var quotas []model.Quota

	usages := asObject(top["usages"])
	keys := make([]string, 0, len(usages))
	for k := range usages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o := asObject(usages[k])
		ratio, ok := asNumber(o["used_ratio"])
		if o == nil || !ok || ratio < 0 {
			continue
		}
		w := unknownWindow()
		var scope *string
		switch k {
		case "limit_5h":
			w.DurationSeconds = i64ptr(5 * 3600)
		case "limit_month_total":
			scope, w.Period = sptr("total"), sptr("month")
		case "limit_month_code":
			scope, w.Period = sptr("code"), sptr("month")
		}
		if t, ok := asTime(o["reset_time"]); ok {
			w.ResetAt = tptr(t)
		}
		quotas = append(quotas, percentQuota(k, scope, ratio*100, w))
	}

	// limits[] is only a fallback representation; with usages present it would duplicate windows.
	if len(quotas) == 0 {
		var limits []json.RawMessage
		_ = json.Unmarshal(top["limits"], &limits)
		seen := map[string]bool{}
		for _, raw := range limits {
			if q, ok := kimiLimit(asObject(raw)); ok && !seen[q.ID] {
				seen[q.ID] = true
				quotas = append(quotas, q)
			}
		}
	}
	return finish(observed, nil, quotas, nil, body)
}

func kimiLimit(o map[string]json.RawMessage) (model.Quota, bool) {
	detail := asObject(o["detail"])
	if detail == nil {
		return model.Quota{}, false
	}
	limit, okL := asNumber(detail["limit"])
	if !okL || limit <= 0 {
		return model.Quota{}, false
	}
	var used float64
	if rem, ok := asNumber(detail["remaining"]); ok {
		used = (limit - rem) / limit * 100
	} else if u, ok := asNumber(detail["used"]); ok {
		used = u / limit * 100
	} else {
		return model.Quota{}, false
	}
	w := unknownWindow()
	id := ""
	for _, key := range []string{"id", "name", "title"} {
		if text, ok := asString(o[key]); ok && slug(text) != "" {
			id = "limit_" + slug(text)
			break
		}
	}
	if win := asObject(o["window"]); win != nil {
		n, okN := asNumber(win["duration"])
		unit, _ := asString(win["timeUnit"])
		unit = strings.TrimPrefix(strings.ToUpper(unit), "TIME_UNIT_")
		mult := map[string]float64{"SECOND": 1, "MINUTE": 60, "HOUR": 3600, "DAY": 86400, "WEEK": 604800}[unit]
		switch {
		case okN && n == 1 && unit == "MONTH":
			id, w.Period = "limit_month", sptr("month")
		case okN && n > 0 && mult > 0 && n*mult < 1e9:
			secs := int64(n * mult)
			w.DurationSeconds = i64ptr(secs)
			switch secs {
			case 18000:
				id = "limit_5h"
			case 604800:
				id, w.Period = "limit_weekly", sptr("week")
			default:
				id = fmt.Sprintf("limit_%ds", secs)
			}
		}
	}
	if id == "" {
		// A position in limits[] is not a stable quota identity.
		return model.Quota{}, false
	}
	if t, ok := asTime(detail["resetTime"]); ok {
		w.ResetAt = tptr(t)
	}
	return percentQuota(id, nil, used, w), true
}
