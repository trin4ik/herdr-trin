package collectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"quota-hub/internal/model"
)

var safeWindowID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

const maxClaudeProxyWindows = 64

type claudeProxyCollector struct{ base }

type claudeProxyWindow struct {
	ID          string   `json:"id"`
	UsedPercent *float64 `json:"used_percent"`
	ResetAt     string   `json:"reset_at,omitempty"`
}

func (w *claudeProxyWindow) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, ok := fields["id"]; !ok {
		return io.ErrUnexpectedEOF
	}
	if _, ok := fields["used_percent"]; !ok {
		return io.ErrUnexpectedEOF
	}
	for key, value := range fields {
		if key != "id" && key != "used_percent" && key != "reset_at" {
			return errors.New("unknown Claude proxy window field")
		}
		if key == "reset_at" && strings.TrimSpace(string(value)) == "null" {
			return errors.New("reset_at must be omitted or an RFC3339 string")
		}
	}
	type wire claudeProxyWindow
	return json.Unmarshal(data, (*wire)(w))
}

type claudeProxyResponse struct {
	SchemaVersion int                 `json:"schema_version"`
	InstanceID    string              `json:"instance_id"`
	Status        string              `json:"status"`
	ObservedAt    string              `json:"observed_at,omitempty"`
	Windows       []claudeProxyWindow `json:"windows"`
}

func (c *claudeProxyCollector) Fetch(ctx context.Context) (model.Result, error) {
	token := strings.TrimSpace(os.Getenv(c.spec.ClaudeProxyTokenEnv))
	if !validHeaderToken(token) {
		return model.Result{}, fail(CodeCredentialsUnavailable)
	}
	r, e := c.get(ctx, c.spec.ClaudeProxyURL, map[string]string{"Authorization": "Bearer " + token}, maxObsBytes)
	if e != nil {
		return model.Result{}, e
	}
	if r.status != http.StatusOK {
		return model.Result{}, c.statusError(r)
	}
	defer zero(r.body)
	var envelope map[string]json.RawMessage
	if json.Unmarshal(r.body, &envelope) != nil {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	for _, key := range []string{"schema_version", "instance_id", "status", "windows"} {
		if _, ok := envelope[key]; !ok {
			return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
		}
	}
	if len(envelope["windows"]) == 0 || envelope["windows"][0] != '[' {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	var payload claudeProxyResponse
	d := json.NewDecoder(bytes.NewReader(r.body))
	d.DisallowUnknownFields()
	if d.Decode(&payload) != nil || payload.SchemaVersion != 1 || payload.InstanceID != c.spec.ID {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	if payload.Status == "no_data" {
		if payload.ObservedAt != "" || len(payload.Windows) != 0 {
			return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
		}
		return model.Result{}, fail(CodeSourceUnavailable)
	}
	if payload.Status != "ok" || len(payload.Windows) == 0 || len(payload.Windows) > maxClaudeProxyWindows || payload.ObservedAt == "" {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	observed, err := time.Parse(time.RFC3339, payload.ObservedAt)
	if err != nil || observed.After(c.now().Add(futureSkew)) || observed.Year() < 2000 || observed.Year() > 2200 {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	quotas := make([]model.Quota, 0, len(payload.Windows))
	seen := make(map[string]bool, len(payload.Windows))
	for _, w := range payload.Windows {
		if !safeWindowID.MatchString(w.ID) || seen[w.ID] {
			return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
		}
		seen[w.ID] = true
		q := model.Quota{ID: w.ID, Metric: "quota", Unit: "percent", Authority: authority}
		q.Window = model.Window{Kind: "unknown"}
		switch w.ID {
		case "five_hour":
			q.Window = model.Window{Kind: "rolling", Period: sptr("5h"), DurationSeconds: i64ptr(5 * 60 * 60)}
		case "seven_day":
			q.Window = model.Window{Kind: "rolling", Period: sptr("7d"), DurationSeconds: i64ptr(7 * 24 * 60 * 60)}
		default:
			q.Window.Period = sptr(w.ID)
		}
		if w.UsedPercent != nil {
			if math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 || *w.UsedPercent > 100 {
				return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
			}
			q.UsedPercent = fptr(*w.UsedPercent)
			q.RemainingPercent = fptr(100 - *w.UsedPercent)
		}
		if w.ResetAt != "" {
			reset, parseErr := time.Parse(time.RFC3339, w.ResetAt)
			if parseErr != nil || reset.Year() < 2000 || reset.Year() > 2200 {
				return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
			}
			q.Window.ResetAt = tptr(reset)
		}
		quotas = append(quotas, q)
	}
	res, err := finish(observed, nil, quotas, nil, r.body)
	if err != nil {
		return model.Result{}, err
	}
	return res, nil
}
