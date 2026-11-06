package collectors

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"quota-hub/internal/model"
)

const (
	claudeHeaderPrefix = "anthropic-ratelimit-unified-"
	observerSource     = "opencode-http-response-hook"
)

var claudeHeaderRE = regexp.MustCompile(`^anthropic-ratelimit-unified-(.+)-(utilization|reset)$`)

type claudeCollector struct {
	base
}

type observedQuota struct {
	Name   string          `json:"name"`
	Family string          `json:"family,omitempty"`
	Type   string          `json:"type"`
	Value  json.RawMessage `json:"value"`
}

type observation struct {
	SchemaVersion int             `json:"schema_version"`
	Source        string          `json:"source"`
	ObservedAt    string          `json:"observed_at"`
	ProviderID    string          `json:"provider_id"`
	Status        int             `json:"status"`
	Quota         []observedQuota `json:"quota"`
}

// Fetch only reads the observation file written by the OpenCode response hook. It makes no
// network request and does not refresh the data: ObservedAt is the response time in the file.
func (c *claudeCollector) Fetch(ctx context.Context) (model.Result, error) {
	if err := ctx.Err(); err != nil {
		return model.Result{}, classifyTransport(ctx, err)
	}
	data, ok := readBounded(c.spec.ObservedFile, maxObsBytes)
	if !ok {
		return model.Result{}, fail(CodeSourceUnavailable)
	}
	var obs observation
	if json.Unmarshal(data, &obs) != nil || obs.SchemaVersion != 1 || obs.Source != observerSource ||
		obs.ProviderID != "anthropic" || !(obs.Status/100 == 2 || obs.Status == http.StatusTooManyRequests) {
		return model.Result{}, fail(CodeInvalidResponse)
	}
	at, err := time.Parse(time.RFC3339Nano, obs.ObservedAt)
	if err != nil || at.After(c.now().Add(futureSkew)) {
		return model.Result{}, fail(CodeInvalidResponse)
	}
	at = at.UTC()

	type window struct {
		util  *float64
		reset *time.Time
	}
	windows := map[string]*window{}
	var order []string
	var kept []observedQuota
	for _, q := range obs.Quota {
		m := claudeHeaderRE.FindStringSubmatch(q.Name)
		if m == nil {
			continue
		}
		w := windows[m[1]]
		if w == nil {
			w = &window{}
			windows[m[1]] = w
			order = append(order, m[1])
		}
		switch m[2] {
		case "utilization":
			if q.Type != "number" {
				continue
			}
			if v, ok := asNumber(q.Value); ok && v >= 0 {
				w.util = fptr(v)
				kept = append(kept, observedQuota{Name: q.Name, Family: q.Family, Type: q.Type, Value: q.Value})
			}
		case "reset":
			if q.Type != "time" {
				continue
			}
			if s, ok := asString(q.Value); ok {
				if t, ok := parseTimeString(s); ok {
					w.reset = tptr(t)
					kept = append(kept, observedQuota{Name: q.Name, Family: q.Family, Type: q.Type, Value: q.Value})
				}
			}
		}
	}

	var quotas []model.Quota
	for _, token := range order {
		w := windows[token]
		if w.util == nil {
			continue
		}
		id, scope, win := claudeWindow(token)
		if id == "" {
			continue
		}
		win.ResetAt = w.reset
		quotas = append(quotas, percentQuota(id, scope, *w.util*100, win))
	}
	if len(quotas) == 0 {
		return model.Result{}, fail(CodeInvalidResponse)
	}
	clean := observation{SchemaVersion: 1, Source: observerSource, ObservedAt: at.Format(time.RFC3339Nano), ProviderID: "anthropic", Status: obs.Status, Quota: kept}
	raw, _ := json.Marshal(clean)
	return finish(at, nil, quotas, nil, raw)
}

// claudeWindow maps a header token (5h, 7d, 7d_sonnet, ...) to id, optional model scope and window.
func claudeWindow(token string) (string, *string, model.Window) {
	w := unknownWindow()
	kind, rest := token, ""
	for _, p := range []string{"5h", "7d"} {
		if token == p || strings.HasPrefix(token, p+"-") || strings.HasPrefix(token, p+"_") {
			kind, rest = p, strings.Trim(token[len(p):], "-_")
		}
	}
	var id string
	switch kind {
	case "5h":
		id, w.DurationSeconds = "five_hour", i64ptr(18000)
	case "7d":
		id, w.DurationSeconds = "seven_day", i64ptr(604800)
		w.Period = sptr("week")
	default:
		return slug(token), nil, w
	}
	if rest == "" {
		return id, nil, w
	}
	return id + "_" + slug(rest), sptr(strings.ReplaceAll(rest, "_", "-")), w
}
