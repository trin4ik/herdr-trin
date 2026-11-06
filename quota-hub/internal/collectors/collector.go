package collectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"quota-hub/internal/model"
)

// New builds a read-only collector for the provider/source pair in spec.
// Provider endpoints are constants; only opts.Endpoints (tests) can replace them.
func New(spec Spec, opts Options) (Collector, error) {
	b := newBase(spec, opts)
	endpoint := func(key, def string) (string, error) {
		if v, ok := opts.Endpoints[key]; ok {
			u, ok := httpEndpoint(v)
			if !ok {
				return "", fail(CodeConfigInvalid)
			}
			return u, nil
		}
		return def, nil
	}
	switch {
	case spec.Provider == ProviderCodex && spec.Source == SourceNativeCodexOAuth:
		if spec.CredentialFile == "" {
			return nil, fail(CodeConfigInvalid)
		}
		ep, err := endpoint("codex", EndpointCodex)
		if err != nil {
			return nil, err
		}
		return &codexCollector{base: b, endpoint: ep}, nil
	case spec.Provider == ProviderOllama && spec.Source == SourceOpenCodeKey:
		oc, err := newOpenCodeKey(spec, opts)
		if err != nil {
			return nil, err
		}
		ep, err := endpoint("ollama", EndpointOllama)
		if err != nil {
			return nil, err
		}
		pep, err := endpoint("ollama_profile", EndpointOllamaProfile)
		if err != nil {
			return nil, err
		}
		return &ollamaCollector{base: b, endpoint: ep, profileEndpoint: pep, oc: oc}, nil
	case spec.Provider == ProviderKimi && spec.Source == SourceOpenCodeKey:
		oc, err := newOpenCodeKey(spec, opts)
		if err != nil {
			return nil, err
		}
		def, pdef := EndpointKimiCN, EndpointKimiCNProfile
		if spec.IntegrationID == KimiGlobalIntegration {
			def, pdef = EndpointKimiGlob, EndpointKimiGlobProfile
		}
		ep, err := endpoint("kimi", def)
		if err != nil {
			return nil, err
		}
		pep, err := endpoint("kimi_profile", pdef)
		if err != nil {
			return nil, err
		}
		return &kimiCollector{base: b, endpoint: ep, profileEndpoint: pep, oc: oc}, nil
	case spec.Provider == ProviderClaude && spec.Source == SourceOpenCodeObserved:
		if spec.ObservedFile == "" {
			return nil, fail(CodeConfigInvalid)
		}
		if spec.ClaudeIdleAfter != 0 {
			ep, err := endpoint("claude", EndpointClaudeUsage)
			if err != nil {
				return nil, err
			}
			h, err := newClaudeHybrid(spec, b, ep)
			if err != nil {
				return nil, err
			}
			return h, nil
		}
		return &claudeCollector{base: b}, nil
	}
	return nil, fail(CodeConfigInvalid)
}

// openCodeKey reads one API key from OpenCode GET /api/credential.
type openCodeKey struct {
	base
	url string
	// file/origin are set instead of url when authentication comes from the
	// OpenCode local-service registration; origin is a test-only override.
	file   string
	origin string
}

func newOpenCodeKey(spec Spec, opts Options) (*openCodeKey, error) {
	if strings.TrimSpace(spec.IntegrationID) == "" {
		return nil, fail(CodeConfigInvalid)
	}
	local := newBase(spec, opts)
	// Never route the secret local credential response through HTTP_PROXY.
	// Provider HTTPS traffic may still use the operator's normal proxy/VPN.
	transport := local.client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if native, ok := transport.(*http.Transport); ok {
		direct := native.Clone()
		direct.Proxy = nil
		local.client.Transport = direct
	}
	if spec.OpenCodeServiceFile != "" {
		if spec.OpenCodeURL != "" || spec.OpenCodeBearerEnv != "" || !filepath.IsAbs(spec.OpenCodeServiceFile) {
			return nil, fail(CodeConfigInvalid)
		}
		o := &openCodeKey{base: local, file: spec.OpenCodeServiceFile}
		if v, ok := opts.Endpoints["opencode"]; ok {
			if o.origin, ok = localOrigin(v); !ok {
				return nil, fail(CodeConfigInvalid)
			}
		}
		return o, nil
	}
	raw := spec.OpenCodeURL
	if v, ok := opts.Endpoints["opencode"]; ok {
		raw = v
	}
	u, ok := httpEndpoint(raw)
	if !ok {
		return nil, fail(CodeConfigInvalid)
	}
	// Only the origin is trusted; the credential path is fixed.
	pu, _ := parseOrigin(u)
	return &openCodeKey{base: local, url: pu + "/api/credential"}, nil
}

func parseOrigin(u string) (string, bool) {
	rest := u
	i := strings.Index(rest, "://")
	if i < 0 {
		return "", false
	}
	scheme := rest[:i]
	rest = rest[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return scheme + "://" + rest, true
}

type credentialEntry struct {
	ID            string          `json:"id"`
	IntegrationID string          `json:"integrationID"`
	Active        bool            `json:"active"`
	Value         json.RawMessage `json:"value"`
}

// key returns the single matching API key; any ambiguity or non-key type is refused.
func (o *openCodeKey) key(ctx context.Context) (string, string, *model.ErrorInfo) {
	headers := map[string]string{}
	target := o.url
	if o.file != "" {
		var auth string
		var e *model.ErrorInfo
		if target, auth, e = o.registration(); e != nil {
			return "", "", e
		}
		if auth != "" {
			headers["Authorization"] = auth
		}
	} else if name := o.spec.OpenCodeBearerEnv; name != "" {
		tok := strings.TrimSpace(os.Getenv(name))
		if !validHeaderToken(tok) {
			return "", "", fail(CodeCredentialsUnavailable)
		}
		headers["Authorization"] = "Bearer " + tok
	}
	r, e := o.get(ctx, target, headers, maxCredBytes)
	if e != nil {
		if e.Code == CodeInvalidResponse || e.Code == CodeConfigInvalid {
			return "", "", fail(CodeCredentialsUnavailable)
		}
		return "", "", e
	}
	if r.status != http.StatusOK {
		// The local credential service may itself throttle or be unavailable.
		// Keep its Retry-After instead of converting every failure to missing keys.
		if r.status == 401 || r.status == 403 || r.status == 429 || r.status >= 500 {
			return "", "", o.statusError(r)
		}
		return "", "", failStatus(CodeCredentialsUnavailable, r.status)
	}
	defer zero(r.body)
	var entries []json.RawMessage
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(r.body, &wrapper) == nil && wrapper.Data != nil {
		if json.Unmarshal(wrapper.Data, &entries) != nil {
			return "", "", fail(CodeCredentialsUnavailable)
		}
	} else if json.Unmarshal(r.body, &entries) != nil {
		return "", "", fail(CodeCredentialsUnavailable)
	}
	var match []credentialEntry
	for _, raw := range entries {
		var en credentialEntry
		if json.Unmarshal(raw, &en) != nil || en.IntegrationID != o.spec.IntegrationID {
			continue
		}
		if o.spec.CredentialID != "" {
			if en.ID != o.spec.CredentialID {
				continue
			}
		} else if !en.Active {
			continue
		}
		match = append(match, en)
	}
	if len(match) != 1 {
		return "", "", fail(CodeCredentialsUnavailable)
	}
	if match[0].ID == "" {
		return "", "", fail(CodeCredentialsUnavailable)
	}
	var v struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if json.Unmarshal(match[0].Value, &v) != nil || v.Type != "key" || !validHeaderToken(v.Key) {
		return "", "", fail(CodeCredentialsUnavailable)
	}
	return v.Key, identity(match[0].IntegrationID + ":" + match[0].ID), nil
}

// fetchKeyed is the shared flow: credential read, then one provider GET.
func fetchKeyed(ctx context.Context, b *base, oc *openCodeKey, endpoint string, parse func(body []byte) (model.Result, error)) (model.Result, error) {
	ctx, cancel := b.context(ctx)
	defer cancel()
	key, account, e := oc.key(ctx)
	if e != nil {
		// The credential source is unverifiable, so no cached profile may be trusted.
		return model.Result{}, withProfileIdentity(e, "", true)
	}
	cred := keyedCredentialIdentity(account, key)
	r, e := b.get(ctx, endpoint, map[string]string{"Authorization": "Bearer " + key}, maxBodyBytes)
	if e != nil {
		return model.Result{}, withProfileIdentity(e, cred, true)
	}
	if r.status != http.StatusOK {
		return model.Result{}, withProfileIdentity(b.statusError(r), cred, true)
	}
	res, err := parse(r.body)
	if err != nil {
		return model.Result{}, withProfileIdentity(err, cred, true)
	}
	res.Identity = account
	res.ProfileIdentity = cred
	return res, nil
}

// keyedCredentialIdentity is a stable private fingerprint of the credential
// record and key value. It is not a quota account identity and is never public.
func keyedCredentialIdentity(account, key string) string {
	return "key:" + identity(account+"\x00"+key)
}

// withProfileIdentity returns a copy of an ErrorInfo with the private credential
// generation attached; other errors pass through unchanged.
func withProfileIdentity(err error, id string, checked bool) error {
	var ei *model.ErrorInfo
	if !errors.As(err, &ei) || ei == nil {
		return err
	}
	c := *ei
	c.ProfileIdentity, c.ProfileIdentityChecked = id, checked
	return &c
}
