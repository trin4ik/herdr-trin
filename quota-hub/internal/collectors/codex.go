package collectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"quota-hub/internal/model"
)

type codexCollector struct {
	base
	endpoint string
}

func (c *codexCollector) MetadataIdentity() (string, error) {
	data, ok := readBounded(c.spec.CredentialFile, maxFileBytes)
	if !ok {
		return "", fail(CodeCredentialsUnavailable)
	}
	defer zero(data)
	var f struct {
		Tokens struct {
			AccountID   string `json:"account_id"`
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &f) != nil || !validHeaderToken(f.Tokens.AccountID) || !validHeaderToken(f.Tokens.AccessToken) {
		return "", fail(CodeCredentialsUnavailable)
	}
	return identity("codex:"+f.Tokens.AccountID) + "." + identity(f.Tokens.AccessToken), nil
}

// Fetch reads access_token/account_id from the native Codex file (never executes the CLI, never refreshes)
// and performs one GET to the usage endpoint.
func (c *codexCollector) Fetch(parent context.Context) (model.Result, error) {
	data, ok := readBounded(c.spec.CredentialFile, maxFileBytes)
	if !ok {
		return model.Result{}, fail(CodeCredentialsUnavailable)
	}
	var f struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	err := json.Unmarshal(data, &f)
	zero(data)
	token, account := f.Tokens.AccessToken, f.Tokens.AccountID
	if err != nil || !validHeaderToken(token) || !validHeaderToken(account) {
		return model.Result{}, fail(CodeCredentialsUnavailable)
	}
	// The JWT is decoded only as an expiry hint; its signature is not validated.
	if exp, ok := jwtExpiry(token); ok && !c.now().Before(exp) {
		return model.Result{}, fail(CodeAuthRequired)
	}
	ctx, cancel := c.context(parent)
	defer cancel()
	r, e := c.get(ctx, c.endpoint, map[string]string{
		"Authorization":      "Bearer " + token,
		"ChatGPT-Account-Id": account,
	}, maxBodyBytes)
	if e != nil {
		return model.Result{}, e
	}
	if r.status != http.StatusOK {
		return model.Result{}, c.statusError(r)
	}
	result, err := parseCodex(r.body, c.now())
	result.Identity = identity("codex:" + account)
	if err == nil {
		result.Profile.Identity = result.Identity + "." + identity(token)
		current, checkErr := c.MetadataIdentity()
		reported, present := asString(asObject(r.body)["account_id"])
		if checkErr != nil {
			result.Profile = nil
		} else if present && reported != account {
			result.Profile = &model.Profile{Invalidated: true}
		} else if current != result.Profile.Identity {
			if strings.HasPrefix(current, result.Identity+".") {
				result.Profile = nil
			} else {
				result.Profile = &model.Profile{Invalidated: true}
			}
		}
	}
	return result, err
}

func jwtExpiry(tok string) (time.Time, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.RawMessage `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return time.Time{}, false
	}
	n, ok := asNumber(claims.Exp)
	if !ok {
		return time.Time{}, false
	}
	return epochTime(n)
}

func parseCodex(body []byte, observed time.Time) (model.Result, error) {
	top := asObject(body)
	if top == nil {
		return model.Result{}, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	var plan *string
	if s, ok := asString(top["plan_type"]); ok && planID.MatchString(s) {
		plan = sptr(s)
	}
	seen := map[string]bool{}
	var quotas []model.Quota
	add := func(obj map[string]json.RawMessage, prefix string, scope *string) {
		for _, pos := range []string{"primary", "secondary"} {
			w, ok := codexWindow(asObject(obj[pos+"_window"]), observed)
			if !ok {
				continue
			}
			id, period := windowIdentity(w.dur, pos)
			if prefix != "" {
				id = prefix + "_" + id
			}
			if seen[id] {
				id += "_" + pos
				if seen[id] {
					continue
				}
			}
			seen[id] = true
			win := unknownWindow()
			win.Period, win.DurationSeconds, win.ResetAt = period, w.dur, w.reset
			quotas = append(quotas, percentQuota(id, scope, w.used, win))
		}
	}
	add(windowHolder(top["rate_limit"]), "", nil)
	add(windowHolder(top["code_review_rate_limit"]), "code_review", sptr("code_review"))
	var extra []json.RawMessage
	if json.Unmarshal(top["additional_rate_limits"], &extra) == nil {
		for _, raw := range extra {
			en := asObject(raw)
			if en == nil {
				continue
			}
			name, _ := asString(en["limit_name"])
			if strings.TrimSpace(name) == "" {
				name, _ = asString(en["metered_feature"])
			}
			s := slug(name)
			if s == "" {
				// An array index is not a stable identity for a quota pool.
				continue
			}
			add(windowHolder(raw), s, sptr(strings.TrimSpace(name)))
		}
	}

	var metrics []model.Metric
	if cr := asObject(top["credits"]); cr != nil {
		unlimited, _ := asBool(cr["unlimited"])
		if bal, ok := asNumber(cr["balance"]); ok && !unlimited && bal >= 0 {
			metrics = append(metrics, model.Metric{ID: "credits_balance", Metric: "balance", Unit: "credits", Value: bal, Authority: authority})
		}
	}
	res, err := finish(observed, plan, quotas, metrics, body)
	email, _ := asString(top["email"])
	res.Profile = &model.Profile{AccountEmail: model.Email(email), Plan: plan}
	return res, err
}

var planID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// windowHolder returns the object that directly holds *_window keys: either the value itself
// or its nested rate_limit object.
func windowHolder(raw json.RawMessage) map[string]json.RawMessage {
	o := asObject(raw)
	if o == nil {
		return nil
	}
	if _, ok := o["primary_window"]; ok {
		return o
	}
	if _, ok := o["secondary_window"]; ok {
		return o
	}
	if n := asObject(o["rate_limit"]); n != nil {
		return n
	}
	return o
}

type codexWin struct {
	used  float64
	dur   *int64
	reset *time.Time
}

func codexWindow(o map[string]json.RawMessage, observed time.Time) (codexWin, bool) {
	var w codexWin
	if o == nil {
		return w, false
	}
	used, ok := asNumber(o["used_percent"])
	if !ok || used < 0 {
		return w, false
	}
	w.used = used
	if d, ok := asNumber(o["limit_window_seconds"]); ok && d > 0 && d < 1e9 {
		w.dur = i64ptr(int64(d))
	}
	if t, ok := asTime(o["reset_at"]); ok {
		w.reset = tptr(t)
	} else if a, ok := asNumber(o["reset_after_seconds"]); ok && a >= 0 && a < 1e9 {
		w.reset = tptr(observed.Add(time.Duration(a) * time.Second))
	}
	return w, true
}

// windowIdentity derives meaning from the reported duration, never from primary/secondary position.
func windowIdentity(dur *int64, fallback string) (string, *string) {
	if dur == nil {
		return fallback, nil
	}
	switch *dur {
	case 18000:
		return "five_hour", nil
	case 604800:
		return "weekly", sptr("week")
	}
	return fmt.Sprintf("window_%ds", *dur), nil
}
