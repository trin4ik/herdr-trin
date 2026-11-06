package collectors

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"quota-hub/internal/model"
)

const maxProfileBytes = 64 << 10

// profileParser extracts untrusted, individually nullable metadata. It fails
// unless the body carries a provider account identifier, so unrelated JSON is
// never mistaken for an authoritative profile with absent fields.
type profileParser func(body []byte) (email, plan *string, err error)

// fetchKeyedProfile performs one optional metadata request with the same
// OpenCode key as usage. The key fingerprint is read before the request and again
// after the response; metadata is returned only if both match expected.
// Failures are returned both as the error and in update.Failure.
func fetchKeyedProfile(ctx context.Context, b *base, oc *openCodeKey, expected, method, endpoint string, parse profileParser) (model.ProfileUpdate, error) {
	ctx, cancel := b.context(ctx)
	defer cancel()
	upd := model.ProfileUpdate{ExpectedIdentity: expected}
	reject := func(e *model.ErrorInfo) (model.ProfileUpdate, error) {
		upd.Profile, upd.Failure = nil, e
		return upd, e
	}

	key, account, e := oc.key(ctx)
	upd.IdentityChecked = true
	if e != nil {
		return reject(e)
	}
	before := keyedCredentialIdentity(account, key)
	upd.CurrentIdentity = before
	if before != expected {
		return reject(fail(CodeProfileIdentityChanged))
	}

	var email, plan *string
	upstream := func() *model.ErrorInfo {
		r, e := b.request(ctx, method, endpoint, map[string]string{"Authorization": "Bearer " + key}, maxProfileBytes)
		if e != nil {
			return e
		}
		if r.status != http.StatusOK {
			return b.statusError(r)
		}
		var err error
		if email, plan, err = parse(r.body); err != nil {
			if ei, ok := err.(*model.ErrorInfo); ok {
				return ei
			}
			return failStatus(CodeInvalidResponse, http.StatusOK)
		}
		return nil
	}()

	key2, account2, e2 := oc.key(ctx)
	switch {
	case e2 != nil:
		upd.CurrentIdentity = ""
	default:
		upd.CurrentIdentity = keyedCredentialIdentity(account2, key2)
	}
	switch {
	case upstream != nil && upstream.RetryAt != nil:
		// A remote cooldown must survive a concurrent credential change.
		return reject(upstream)
	case e2 != nil:
		return reject(e2)
	case upd.CurrentIdentity != before:
		return reject(fail(CodeProfileIdentityChanged))
	case upstream != nil:
		return reject(upstream)
	}
	upd.Profile = &model.Profile{Identity: before, AccountEmail: email, Plan: plan}
	return upd, nil
}

// profileField returns the first present key; the empty result means absent.
func profileField(top map[string]json.RawMessage, names ...string) json.RawMessage {
	for _, n := range names {
		if v, ok := top[n]; ok {
			return v
		}
	}
	return nil
}

func parseProfileObject(body []byte, idKeys, emailKeys, planKeys []string) (email, plan *string, err error) {
	top := asObject(body)
	if top == nil {
		return nil, nil, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	if id, ok := asString(profileField(top, idKeys...)); !ok || strings.TrimSpace(id) == "" {
		return nil, nil, failStatus(CodeInvalidResponse, http.StatusOK)
	}
	if s, ok := asString(profileField(top, emailKeys...)); ok {
		email = model.Email(s)
	}
	if s, ok := asString(profileField(top, planKeys...)); ok {
		plan = model.ProfileText(s, 100)
	}
	return email, plan, nil
}

// Ollama POST /api/me answers PascalCase live; lowercase is the documented form.
func parseOllamaProfile(body []byte) (*string, *string, error) {
	return parseProfileObject(body, []string{"ID", "id"}, []string{"Email", "email"}, []string{"Plan", "plan"})
}

// Kimi GET /coding/v1/me. The plan is only the reported level name.
func parseKimiProfile(body []byte) (*string, *string, error) {
	return parseProfileObject(body, []string{"user_id"}, []string{"email"}, []string{"user_level_name"})
}
