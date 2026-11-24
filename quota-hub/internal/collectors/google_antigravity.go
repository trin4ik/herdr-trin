package collectors

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"quota-hub/internal/model"
)

// googleAntigravityCollector reads the active Antigravity OAuth access token
// from the local OpenCode credential API and polls Google's read-only quota
// summary endpoint. It never reads the refresh token and never refreshes auth.
type googleAntigravityCollector struct {
	base
	oc              *openCodeKey
	endpoint        string
	profileEndpoint string
}

type googleCredential struct {
	token           string
	accountIdentity string
	profileIdentity string
	email           *string
}

var googleLoadCodeAssistPayload = []byte(`{"metadata":{"ideType":"ANTIGRAVITY"}}`)

func (c *googleAntigravityCollector) googleCredential(ctx context.Context) (googleCredential, *model.ErrorInfo) {
	body, e := c.oc.rawCredentials(ctx)
	if e != nil {
		return googleCredential{}, e
	}
	defer zero(body)
	var entries []json.RawMessage
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &wrapper) == nil && wrapper.Data != nil {
		if json.Unmarshal(wrapper.Data, &entries) != nil {
			return googleCredential{}, fail(CodeCredentialsUnavailable)
		}
	} else if json.Unmarshal(body, &entries) != nil {
		return googleCredential{}, fail(CodeCredentialsUnavailable)
	}
	var match []credentialEntry
	for _, raw := range entries {
		var en credentialEntry
		if json.Unmarshal(raw, &en) != nil || en.IntegrationID != c.spec.IntegrationID {
			continue
		}
		if c.spec.CredentialID != "" {
			if en.ID != c.spec.CredentialID {
				continue
			}
		} else if !en.Active {
			continue
		}
		match = append(match, en)
	}
	if len(match) != 1 || match[0].ID == "" {
		return googleCredential{}, fail(CodeCredentialsUnavailable)
	}
	entry := match[0]
	var v struct {
		Type     string          `json:"type"`
		Access   string          `json:"access"`
		Expires  json.RawMessage `json:"expires"`
		MethodID json.RawMessage `json:"methodID"`
		Metadata struct {
			AccountID json.RawMessage `json:"antigravityAccountId"`
			Email     json.RawMessage `json:"email"`
		} `json:"metadata"`
	}
	if json.Unmarshal(entry.Value, &v) != nil || v.Type != "oauth" {
		return googleCredential{}, fail(CodeCredentialsUnavailable)
	}
	method, ok := asString(v.MethodID)
	if !ok || method != "antigravity" {
		return googleCredential{}, fail(CodeCredentialsUnavailable)
	}
	if len(v.Expires) > 0 && string(v.Expires) != "null" {
		n, ok := asNumber(v.Expires)
		if !ok || n <= 0 || n > float64(time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()) || math.Trunc(n) != n {
			return googleCredential{}, fail(CodeCredentialsUnavailable)
		}
		if !c.now().Before(time.UnixMilli(int64(n)).UTC()) {
			return googleCredential{}, fail(CodeAuthRequired)
		}
	}
	if !validHeaderToken(v.Access) {
		return googleCredential{}, fail(CodeCredentialsUnavailable)
	}

	accountMeta, _ := asString(v.Metadata.AccountID)
	if !validHeaderToken(accountMeta) {
		accountMeta = ""
	}
	emailStr, _ := asString(v.Metadata.Email)
	email := model.Email(emailStr)

	accountID := "google:" + identity(entry.IntegrationID+":"+entry.ID)
	profileID := keyedCredentialIdentity(accountID, v.Access)
	if accountMeta != "" {
		accountID = "google:" + identity(entry.ID+"\x00"+accountMeta)
		emailKey := ""
		if email != nil {
			emailKey = *email
		}
		profileID = "google:" + identity(entry.ID+"\x00"+accountMeta+"\x00"+emailKey)
	}
	return googleCredential{
		token:           v.Access,
		accountIdentity: accountID,
		profileIdentity: profileID,
		email:           email,
	}, nil
}

func (c *googleAntigravityCollector) Fetch(ctx context.Context) (model.Result, error) {
	ctx, cancel := c.context(ctx)
	defer cancel()
	cred, e := c.googleCredential(ctx)
	if e != nil {
		return model.Result{}, withProfileIdentity(e, "", true)
	}
	headers := map[string]string{
		"Authorization": "Bearer " + cred.token,
		"User-Agent":    googleAntigravityUserAgent,
	}
	r, e := c.postJSON(ctx, c.endpoint, headers, []byte("{}"), maxBodyBytes)
	if e != nil {
		return model.Result{}, withProfileIdentity(e, cred.profileIdentity, true)
	}
	if r.status != http.StatusOK {
		id := cred.profileIdentity
		if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
			id = ""
		}
		return model.Result{}, withProfileIdentity(c.statusError(r), id, true)
	}
	observed := c.now().UTC()
	quotas, err := parseGoogleQuotaSummary(r.body, observed)
	if err != nil {
		return model.Result{}, withProfileIdentity(err, cred.profileIdentity, true)
	}
	res, err := finish(observed, nil, quotas, nil, r.body)
	if err != nil {
		return model.Result{}, withProfileIdentity(err, cred.profileIdentity, true)
	}
	res.Source = SourceOpenCodeAntigravity
	res.Identity = cred.accountIdentity
	res.ProfileIdentity = cred.profileIdentity
	return res, nil
}

// FetchProfile reads optional account tier metadata via loadCodeAssist using
// the same active Antigravity OAuth token. The credential identity is checked
// before and after the request so concurrent switches never attach stale data.
func (c *googleAntigravityCollector) FetchProfile(ctx context.Context, expected string) (model.ProfileUpdate, error) {
	ctx, cancel := c.context(ctx)
	defer cancel()
	upd := model.ProfileUpdate{ExpectedIdentity: expected}
	reject := func(e *model.ErrorInfo) (model.ProfileUpdate, error) {
		upd.Profile, upd.Failure = nil, e
		return upd, e
	}

	cred, e := c.googleCredential(ctx)
	upd.IdentityChecked = true
	if e != nil {
		return reject(e)
	}
	upd.CurrentIdentity = cred.profileIdentity
	if cred.profileIdentity != expected {
		return reject(fail(CodeProfileIdentityChanged))
	}

	var plan *string
	upstream := func() *model.ErrorInfo {
		headers := map[string]string{
			"Authorization": "Bearer " + cred.token,
			"User-Agent":    googleAntigravityUserAgent,
		}
		r, e := c.postJSON(ctx, c.profileEndpoint, headers, googleLoadCodeAssistPayload, maxProfileBytes)
		if e != nil {
			return e
		}
		if r.status != http.StatusOK {
			return c.statusError(r)
		}
		var err error
		if plan, err = parseGoogleProfile(r.body); err != nil {
			if ei, ok := err.(*model.ErrorInfo); ok {
				return ei
			}
			return failStatus(CodeInvalidResponse, http.StatusOK)
		}
		return nil
	}()

	cred2, e2 := c.googleCredential(ctx)
	if e2 != nil {
		upd.CurrentIdentity = ""
	} else {
		upd.CurrentIdentity = cred2.profileIdentity
	}
	switch {
	case upstream != nil && upstream.RetryAt != nil:
		return reject(upstream)
	case e2 != nil:
		return reject(e2)
	case upd.CurrentIdentity != cred.profileIdentity:
		return reject(fail(CodeProfileIdentityChanged))
	case upstream != nil:
		return reject(upstream)
	}
	upd.Profile = &model.Profile{Identity: cred.profileIdentity, AccountEmail: cred.email, Plan: plan}
	return upd, nil
}

func parseGoogleProfile(body []byte) (*string, error) {
	top := asObject(body)
	if top == nil {
		return nil, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	paid := asObject(top["paidTier"])
	current := asObject(top["currentTier"])
	_, hasProject := top["cloudaicompanionProject"]
	if paid == nil && current == nil && !hasProject {
		return nil, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	if paid != nil {
		if name, ok := asString(paid["name"]); ok {
			if plan := model.ProfileText(name, 100); plan != nil {
				return plan, nil
			}
		}
		if id, ok := asString(paid["id"]); ok {
			if mapped := googlePaidTierLabel(id); mapped != nil {
				return mapped, nil
			}
		}
	}
	if current != nil {
		if name, ok := asString(current["name"]); ok {
			if plan := model.ProfileText(name, 100); plan != nil {
				return plan, nil
			}
		}
	}
	return nil, nil
}

func googlePaidTierLabel(id string) *string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "g1-pro-tier":
		return sptr("Google AI Pro")
	case "g1-ultra-tier":
		return sptr("Google AI Ultra")
	case "g1-ultra-lite-tier":
		return sptr("Google AI Ultra Lite")
	default:
		return nil
	}
}

type orderedGoogleQuota struct {
	priority int
	index    int
	quota    model.Quota
}

func parseGoogleQuotaSummary(body []byte, observed time.Time) ([]model.Quota, error) {
	top := asObject(body)
	if top == nil {
		return nil, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	rawGroups, ok := top["groups"]
	if !ok {
		if env := asObject(top["response"]); env != nil {
			rawGroups, ok = env["groups"]
		}
	}
	if !ok {
		return nil, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	var groups []json.RawMessage
	if json.Unmarshal(rawGroups, &groups) != nil {
		return nil, failStatus(CodeInvalidResponse, http.StatusOK)
	}

	seen := map[string]bool{}
	var ordered []orderedGoogleQuota
	idx := 0
	for _, rawGroup := range groups {
		gObj := asObject(rawGroup)
		if gObj == nil {
			return nil, failStatus(CodeInvalidResponse, http.StatusOK)
		}
		groupName, _ := asString(gObj["displayName"])
		rawBuckets, hasBuckets := gObj["buckets"]
		if !hasBuckets {
			continue
		}
		var buckets []json.RawMessage
		if json.Unmarshal(rawBuckets, &buckets) != nil {
			return nil, failStatus(CodeInvalidResponse, http.StatusOK)
		}
		for _, rawBucket := range buckets {
			bObj := asObject(rawBucket)
			if bObj == nil {
				return nil, failStatus(CodeInvalidResponse, http.StatusOK)
			}
			if rawDisabled, hasDisabled := bObj["disabled"]; hasDisabled {
				disabled, ok := asBool(rawDisabled)
				if !ok {
					return nil, failStatus(CodeInvalidResponse, http.StatusOK)
				}
				if disabled {
					continue
				}
			}

			bucketID, _ := asString(bObj["bucketId"])
			bucketLabel, _ := asString(bObj["displayName"])
			winStr, _ := asString(bObj["window"])

			var resetAt *time.Time
			if rawReset, hasReset := bObj["resetTime"]; hasReset && string(rawReset) != "null" && string(rawReset) != `""` {
				t, ok := asTime(rawReset)
				if !ok {
					return nil, failStatus(CodeInvalidResponse, http.StatusOK)
				}
				resetAt = tptr(t)
			}

			remainingFraction, measured, err := parseGoogleRemainingFraction(bObj, resetAt != nil)
			if err != nil {
				return nil, err
			}
			if !measured {
				continue
			}

			q, prio := mapGoogleBucket(bucketID, groupName, bucketLabel, winStr, remainingFraction, resetAt, observed)
			if q.ID == "" {
				return nil, failStatus(CodeInvalidResponse, http.StatusOK)
			}
			if seen[q.ID] {
				return nil, failStatus(CodeInvalidResponse, http.StatusOK)
			}
			seen[q.ID] = true
			ordered = append(ordered, orderedGoogleQuota{priority: prio, index: idx, quota: q})
			idx++
		}
	}
	if len(ordered) == 0 {
		return nil, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].priority != ordered[j].priority {
			return ordered[i].priority < ordered[j].priority
		}
		return ordered[i].index < ordered[j].index
	})
	quotas := make([]model.Quota, len(ordered))
	for i, item := range ordered {
		quotas[i] = item.quota
	}
	return quotas, nil
}

// parseGoogleRemainingFraction reads remainingFraction (top-level or nested
// under remaining.remainingFraction). Proto3 JSON omits finite 0.0 when a
// bucket is exhausted, so an absent field with a valid resetTime is treated as
// 0% remaining (100% used); an empty bucket without resetTime is skipped.
func parseGoogleRemainingFraction(bucket map[string]json.RawMessage, hasReset bool) (float64, bool, error) {
	raw, exists := bucket["remainingFraction"]
	if !exists {
		if rawRem, hasRem := bucket["remaining"]; hasRem && string(rawRem) != "null" {
			remObj := asObject(rawRem)
			if remObj == nil {
				return 0, false, failStatus(CodeInvalidResponse, http.StatusOK)
			}
			raw, exists = remObj["remainingFraction"]
		}
	}
	if !exists {
		if !hasReset {
			return 0, false, nil
		}
		return 0, true, nil
	}
	frac, ok := asNumber(raw)
	if !ok || frac < 0 || frac > 1 {
		return 0, false, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	return frac, true, nil
}

func mapGoogleBucket(bucketID, groupName, bucketLabel, winStr string, remainingFraction float64, resetAt *time.Time, observed time.Time) (model.Quota, int) {
	rawID := strings.ToLower(strings.TrimSpace(bucketID))
	win, cadenceKnown := googleBucketWindow(winStr, rawID, resetAt)

	id := slug(rawID)
	if id == "" {
		id = slug(groupName + "_" + winStr)
	}

	var scope *string
	var modelFilter *string
	priority := 60

	switch {
	case rawID == "gemini-5h":
		id = "gemini_5h"
		scope = sptr("Gemini")
		modelFilter = sptr("gemini")
		priority = 0
	case rawID == "gemini-weekly":
		id = "gemini_weekly"
		scope = sptr("Gemini")
		modelFilter = sptr("gemini")
		priority = 10
	case strings.Contains(rawID, "argon") || strings.Contains(strings.ToLower(groupName), "argon"):
		scope = cleanGoogleScope(groupName, "Gemini 4 Argon")
		modelFilter = sptr("argon")
		priority = 20
	case strings.HasPrefix(rawID, "gemini-"):
		scope = cleanGoogleScope(groupName, "Gemini")
		modelFilter = sptr("gemini")
		priority = 25
	case rawID == "3p-5h":
		id = "3p_5h"
		scope = sptr("Claude / GPT")
		modelFilter = sptr("claude,gpt")
		priority = 30
	case rawID == "3p-weekly":
		id = "3p_weekly"
		scope = sptr("Claude / GPT")
		modelFilter = sptr("claude,gpt")
		priority = 40
	case strings.HasPrefix(rawID, "3p-"):
		scope = sptr("Claude / GPT")
		modelFilter = sptr("claude,gpt")
		priority = 50
	default:
		scope = cleanGoogleScope(groupName, "")
		if win.DurationSeconds != nil && *win.DurationSeconds == 18000 {
			priority = 55
		}
	}

	usedPct := (1 - remainingFraction) * 100
	q := percentQuota(id, scope, usedPct, win)
	limited := true
	q.Limited = &limited
	q.ObservedAt = tptr(observed)
	q.Model = modelFilter
	if !cadenceKnown {
		if label := model.ProfileText(bucketLabel, 120); label != nil {
			q.Label = label
		}
	}
	return q, priority
}

func googleBucketWindow(winStr, rawID string, resetAt *time.Time) (model.Window, bool) {
	w := unknownWindow()
	w.ResetAt = resetAt
	token := strings.ToLower(strings.TrimSpace(winStr))
	if token == "" {
		switch {
		case strings.HasSuffix(rawID, "-5h"):
			token = "5h"
		case strings.HasSuffix(rawID, "-weekly"):
			token = "weekly"
		case strings.HasSuffix(rawID, "-daily"):
			token = "daily"
		case strings.HasSuffix(rawID, "-monthly"):
			token = "monthly"
		}
	}
	switch token {
	case "5h", "five_hour", "five-hour", "session":
		w.DurationSeconds = i64ptr(18000)
		return w, true
	case "weekly", "week", "7d":
		w.Period = sptr("week")
		w.DurationSeconds = i64ptr(604800)
		return w, true
	case "daily", "day", "24h":
		w.Period = sptr("day")
		w.DurationSeconds = i64ptr(86400)
		return w, true
	case "monthly", "month":
		w.Period = sptr("month")
		return w, true
	default:
		return w, false
	}
}

func cleanGoogleScope(groupName, fallback string) *string {
	if s := model.ProfileText(groupName, 80); s != nil {
		return s
	}
	if fallback != "" {
		return sptr(fallback)
	}
	return nil
}
