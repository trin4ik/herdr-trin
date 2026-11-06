package collectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"quota-hub/internal/model"
)

const (
	oauthTestToken   = "fixture-oauth-access-not-a-real-token"
	oauthTestRefresh = "fixture-oauth-refresh-not-a-real-token"
	oauthTestUUID    = "11111111-2222-3333-4444-555555555555"
	oauthTestEmail   = "someone@example.invalid"
)

const oauthUsageBody = `{
  "five_hour": {"utilization": 22, "resets_at": "2026-11-04T14:00:00.123456+00:00"},
  "seven_day": {"utilization": 41.5, "resets_at": "2026-11-09T00:00:00+00:00"},
  "seven_day_opus": null,
  "seven_day_sonnet": {"utilization": 7, "resets_at": null},
  "seven_day_oauth_apps": {"utilization": 0},
  "seven_day_cowork": {"utilization": 3, "resets_at": "2026-11-09T00:00:00Z"},
  "five_hour_future-pool": {"utilization": 120},
  "iguana_necktie": {"utilization": 99},
  "limits": [{"id":"five_hour","utilization":22},{"id":"seven_day","utilization":41.5}],
  "extra_usage": {"is_enabled": true, "monthly_limit": 5000, "used_credits": 12.5, "utilization": 0.25}
}`

func writeOAuthCreds(t *testing.T, expiresMillis int64) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      oauthTestToken,
			"refreshToken":     oauthTestRefresh,
			"expiresAt":        expiresMillis,
			"scopes":           []string{"user:inference"},
			"subscriptionType": "max",
		},
		"mcpOAuth": map[string]any{"x": "ignored"},
	})
	return writeTemp(t, "credentials.json", b)
}

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeAccountFile(t *testing.T, uuid string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"numStartups":  3,
		"oauthAccount": map[string]any{"accountUuid": uuid, "emailAddress": oauthTestEmail, "organizationName": "Private Org"},
		"projects":     map[string]any{"/some/path": map[string]any{}},
	})
	return writeTemp(t, ".claude.json", b)
}

func pinOf(uuid string) string {
	s := sha256.Sum256([]byte(uuid))
	return hex.EncodeToString(s[:])
}

func hybridSpec(creds, observed string) Spec {
	return Spec{
		Definition:      model.Definition{ID: "claude", Provider: ProviderClaude, Source: SourceOpenCodeObserved},
		ObservedFile:    observed,
		CredentialFile:  creds,
		ClaudeIdleAfter: 30 * time.Minute,
		ObserveInterval: time.Minute,
	}
}

func hybridFetcher(t *testing.T, spec Spec, srv *counted) HybridCollector {
	t.Helper()
	eps := map[string]string{}
	if srv != nil {
		eps["claude"] = srv.URL + "/api/oauth/usage"
	}
	c, err := New(spec, opts(eps))
	if err != nil {
		t.Fatal(err)
	}
	h, ok := c.(HybridCollector)
	if !ok {
		t.Fatalf("%T is not a HybridCollector", c)
	}
	return h
}

func TestClaudeOAuthSuccess(t *testing.T) {
	var gotReq *http.Request
	var paths []string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotReq = r.Clone(context.Background())
		paths = append(paths, r.URL.Path)
		jsonHandler([]byte(oauthUsageBody))(w, r)
	})
	creds := writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli())
	before, _ := os.ReadFile(creds)
	h := hybridFetcher(t, hybridSpec(creds, filepath.Join(t.TempDir(), "absent.json")), srv)
	res, err := h.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.Method != http.MethodGet || gotReq.URL.Path != "/api/oauth/usage" || gotReq.URL.RawQuery != "" ||
		gotReq.Header.Get("Authorization") != "Bearer "+oauthTestToken ||
		gotReq.Header.Get("anthropic-beta") != "oauth-2025-04-20" ||
		gotReq.Header.Get("Accept") != "application/json" || gotReq.Header.Get("User-Agent") != "quota-hub/0.1" {
		t.Fatalf("request %s %s %v", gotReq.Method, gotReq.URL, gotReq.Header)
	}
	if srv.hits.Load() != 1 || len(paths) != 1 {
		t.Fatalf("hits %d", srv.hits.Load())
	}
	if res.Source != "claude_code_oauth" || res.Identity != "" {
		t.Fatalf("source=%q identity=%q", res.Source, res.Identity)
	}
	if !res.ObservedAt.Equal(fakeNow) || res.Plan != nil || len(res.Metrics) != 0 {
		t.Fatalf("meta %+v", res)
	}
	if string(res.Raw) != oauthUsageBody {
		t.Fatal("raw body is not the exact response")
	}
	// utilization is already percent used: 22 stays 22.
	f := quotaByID(t, res, "five_hour")
	if !approx(f.UsedPercent, 22) || !approx(f.RemainingPercent, 78) || *f.Window.DurationSeconds != 18000 || f.Scope != nil ||
		f.Window.ResetAt == nil || !f.Window.ResetAt.Equal(time.Date(2026, 11, 4, 14, 0, 0, 123456000, time.UTC)) {
		t.Fatalf("%+v", f)
	}
	if f.Used != nil || f.Limit != nil || f.Remaining != nil {
		t.Fatal("absolute values must stay null")
	}
	s := quotaByID(t, res, "seven_day")
	if !approx(s.UsedPercent, 41.5) || *s.Window.DurationSeconds != 604800 || s.Window.ResetAt == nil {
		t.Fatalf("%+v", s)
	}
	if so := quotaByID(t, res, "seven_day_sonnet"); !approx(so.UsedPercent, 7) || *so.Scope != "sonnet" || so.Window.ResetAt != nil {
		t.Fatalf("%+v", so)
	}
	if oa := quotaByID(t, res, "seven_day_oauth_apps"); !approx(oa.UsedPercent, 0) || *oa.Scope != "oauth-apps" {
		t.Fatalf("%+v", oa)
	}
	if cw := quotaByID(t, res, "seven_day_cowork"); !approx(cw.UsedPercent, 3) || *cw.Scope != "cowork" {
		t.Fatalf("%+v", cw)
	}
	// overage is kept, not clamped.
	if fp := quotaByID(t, res, "five_hour_future_pool"); !approx(fp.UsedPercent, 120) || !approx(fp.RemainingPercent, -20) || *fp.Scope != "future-pool" {
		t.Fatalf("%+v", fp)
	}
	// null window is not invented; limits[], spend and unknown families are not quotas.
	if len(res.Quotas) != 6 {
		t.Fatalf("quotas %v", ids(res))
	}
	for _, q := range res.Quotas {
		if q.ID == "seven_day_opus" || strings.Contains(q.ID, "iguana") || strings.Contains(q.ID, "limits") {
			t.Fatalf("unexpected %s", q.ID)
		}
	}
	after, _ := os.ReadFile(creds)
	if !bytes.Equal(before, after) {
		t.Fatal("credential file modified")
	}
}

func TestClaudeOAuthRefreshTokenNeverSent(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if dump := fmt.Sprint(r.URL.String(), r.Header); strings.Contains(dump, oauthTestRefresh) || r.URL.Path != "/api/oauth/usage" {
			t.Errorf("unexpected request %s %v", r.URL, r.Header)
		}
		jsonHandler([]byte(oauthUsageBody))(w, r)
	})
	h := hybridFetcher(t, hybridSpec(writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli()), filepath.Join(t.TempDir(), "o.json")), srv)
	if _, err := h.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a rejected token must not trigger any second request (no refresh, no retry).
	rej := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	h = hybridFetcher(t, hybridSpec(writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli()), filepath.Join(t.TempDir(), "o.json")), rej)
	_, err := h.Fetch(context.Background())
	if ei := errInfo(t, err); ei.Code != CodeAuthRequired || ei.HTTPStatus != 401 || rej.hits.Load() != 1 {
		t.Fatalf("%+v hits=%d", ei, rej.hits.Load())
	}
}

func TestClaudeOAuthBadCredentialsNoNetwork(t *testing.T) {
	srv := serve(t, jsonHandler([]byte(oauthUsageBody)))
	raw := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return writeTemp(t, "c.json", b)
	}
	oauth := func(m map[string]any) string { return raw(map[string]any{"claudeAiOauth": m}) }
	big := writeTemp(t, "big.json", bytes.Repeat([]byte(" "), maxFileBytes+10))
	fifoDir := t.TempDir()
	fifo := filepath.Join(fifoDir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	future := fakeNow.Add(time.Hour).UnixMilli()
	cases := map[string]struct{ file, code string }{
		"missing":          {filepath.Join(t.TempDir(), "none.json"), CodeCredentialsUnavailable},
		"directory":        {t.TempDir(), CodeCredentialsUnavailable},
		"fifo":             {fifo, CodeCredentialsUnavailable},
		"oversized":        {big, CodeCredentialsUnavailable},
		"garbage":          {writeTemp(t, "g.json", []byte("nope")), CodeCredentialsUnavailable},
		"other family":     {raw(map[string]any{"mcpOAuth": map[string]any{"accessToken": "x"}}), CodeCredentialsUnavailable},
		"empty token":      {oauth(map[string]any{"accessToken": "", "expiresAt": future}), CodeCredentialsUnavailable},
		"space token":      {oauth(map[string]any{"accessToken": "to ken", "expiresAt": future}), CodeCredentialsUnavailable},
		"bad expiry":       {oauth(map[string]any{"accessToken": "tok", "expiresAt": "soon"}), CodeCredentialsUnavailable},
		"expired":          {oauth(map[string]any{"accessToken": "tok", "expiresAt": fakeNow.Add(-time.Hour).UnixMilli()}), CodeAuthRequired},
		"expired just now": {oauth(map[string]any{"accessToken": "tok", "expiresAt": fakeNow.UnixMilli()}), CodeAuthRequired},
		"refresh only":     {oauth(map[string]any{"refreshToken": oauthTestRefresh, "expiresAt": future}), CodeCredentialsUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := hybridFetcher(t, hybridSpec(tc.file, filepath.Join(t.TempDir(), "o.json")), srv)
			_, err := h.Fetch(context.Background())
			if ei := errInfo(t, err); ei.Code != tc.code {
				t.Fatalf("got %q", ei.Code)
			}
		})
	}
	if srv.hits.Load() != 0 {
		t.Fatalf("requests with bad credentials: %d", srv.hits.Load())
	}
}

func TestClaudeOAuthAccountPin(t *testing.T) {
	srv := serve(t, jsonHandler([]byte(oauthUsageBody)))
	creds := writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli())
	spec := func(accountFile, pin string) Spec {
		s := hybridSpec(creds, filepath.Join(t.TempDir(), "o.json"))
		s.ClaudeAccountFile, s.ClaudeExpectedAccountSHA256 = accountFile, pin
		return s
	}
	acct := writeAccountFile(t, oauthTestUUID)
	acctBefore, _ := os.ReadFile(acct)

	// matching pin (upper-case hex accepted) reaches the provider exactly once.
	h := hybridFetcher(t, spec(acct, strings.ToUpper(pinOf(oauthTestUUID))), srv)
	res, err := h.Fetch(context.Background())
	if err != nil || res.Identity != "" || srv.hits.Load() != 1 {
		t.Fatalf("err=%v id=%q hits=%d", err, res.Identity, srv.hits.Load())
	}

	noUUID := writeTemp(t, "n.json", []byte(`{"oauthAccount":{"emailAddress":"`+oauthTestEmail+`"}}`))
	bad := map[string]string{
		"different account": writeAccountFile(t, "99999999-2222-3333-4444-555555555555"),
		"missing file":      filepath.Join(t.TempDir(), "none.json"),
		"no uuid":           noUUID,
		"garbage":           writeTemp(t, "g.json", []byte("x")),
		"directory":         t.TempDir(),
	}
	for name, file := range bad {
		t.Run(name, func(t *testing.T) {
			before := srv.hits.Load()
			h := hybridFetcher(t, spec(file, pinOf(oauthTestUUID)), srv)
			_, err := h.Fetch(context.Background())
			ei := errInfo(t, err)
			if ei.Code != CodeCredentialsUnavailable || srv.hits.Load() != before {
				t.Fatalf("%+v hits %d->%d", ei, before, srv.hits.Load())
			}
			msg := fmt.Sprintf("%v %+v %#v", err, ei, ei)
			for _, leak := range []string{oauthTestUUID, oauthTestEmail, "Private Org", file, oauthTestToken} {
				if strings.Contains(msg, leak) {
					t.Fatalf("error leaks %q", leak)
				}
			}
		})
	}
	// mismatched pin is checked before the credential file is even read.
	h = hybridFetcher(t, func() Spec {
		s := spec(acct, pinOf("someone-else"))
		s.CredentialFile = filepath.Join(t.TempDir(), "none.json")
		return s
	}(), srv)
	if _, err := h.Fetch(context.Background()); errInfo(t, err).Code != CodeCredentialsUnavailable {
		t.Fatal("pin")
	}
	after, _ := os.ReadFile(acct)
	if !bytes.Equal(acctBefore, after) {
		t.Fatal("account file modified")
	}

	// the pin can never be silently ineffective.
	invalid := []Spec{
		spec("", pinOf(oauthTestUUID)),
		spec(acct, "abc"),
		spec(acct, strings.Repeat("z", 64)),
		spec(acct, pinOf(oauthTestUUID)+"00"),
	}
	for i, s := range invalid {
		if _, err := New(s, opts(nil)); err == nil || errInfo(t, err).Code != CodeConfigInvalid {
			t.Fatalf("invalid pin config %d accepted", i)
		}
	}
	// a large .claude.json (project history) is still readable.
	large := writeTemp(t, "large.json", []byte(fmt.Sprintf(`{"junk":%q,"oauthAccount":{"accountUuid":%q}}`, strings.Repeat("a", 3<<20), oauthTestUUID)))
	h = hybridFetcher(t, spec(large, pinOf(oauthTestUUID)), srv)
	if _, err := h.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeOAuthStatusMapping(t *testing.T) {
	retryDate := fakeNow.Add(5 * time.Hour).UTC().Format(http.TimeFormat)
	cases := []struct {
		name   string
		status int
		retry  string
		code   string
		at     *time.Time
	}{
		{"401", 401, "", CodeAuthRequired, nil},
		{"403", 403, "", CodeAccessDenied, nil},
		{"429 seconds", 429, "120", CodeRateLimited, tptr(fakeNow.Add(120 * time.Second))},
		{"429 date", 429, retryDate, CodeRateLimited, tptr(fakeNow.Add(5 * time.Hour))},
		{"429 none", 429, "", CodeRateLimited, nil},
		{"503", 503, "30", CodeUpstreamError, tptr(fakeNow.Add(30 * time.Second))},
		{"500", 500, "30", CodeUpstreamError, nil},
		{"404", 404, "", CodeInvalidResponse, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.retry != "" {
					w.Header().Set("Retry-After", tc.retry)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"error":"secret detail"}`))
			})
			h := hybridFetcher(t, hybridSpec(writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli()), filepath.Join(t.TempDir(), "o.json")), srv)
			_, err := h.Fetch(context.Background())
			ei := errInfo(t, err)
			if ei.Code != tc.code || ei.HTTPStatus != tc.status || srv.hits.Load() != 1 {
				t.Fatalf("%+v hits=%d", ei, srv.hits.Load())
			}
			if (tc.at == nil) != (ei.RetryAt == nil) || (tc.at != nil && !ei.RetryAt.Equal(*tc.at)) {
				t.Fatalf("retry %v want %v", ei.RetryAt, tc.at)
			}
		})
	}
}

func TestClaudeOAuthRedirectNotFollowed(t *testing.T) {
	other := serve(t, jsonHandler([]byte(oauthUsageBody)))
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	})
	h := hybridFetcher(t, hybridSpec(writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli()), filepath.Join(t.TempDir(), "o.json")), srv)
	_, err := h.Fetch(context.Background())
	if ei := errInfo(t, err); ei.Code != CodeInvalidResponse || other.hits.Load() != 0 || srv.hits.Load() != 1 {
		t.Fatalf("%+v other=%d", ei, other.hits.Load())
	}
}

func TestClaudeOAuthUnsupportedShapes(t *testing.T) {
	bodies := map[string]string{
		"limits only":        `{"limits":[{"id":"five_hour","utilization":22}],"spend":{"x":1}}`,
		"array":              `[{"utilization":1}]`,
		"empty":              `{}`,
		"all null":           `{"five_hour":null,"seven_day":null}`,
		"missing utilizaton": `{"five_hour":{"resets_at":"2026-11-04T14:00:00Z"}}`,
		"null utilization":   `{"five_hour":{"utilization":null}}`,
		"negative":           `{"five_hour":{"utilization":-1}}`,
		"string object":      `{"five_hour":"22"}`,
		"unknown family":     `{"monthly":{"utilization":5}}`,
		"not json":           `<html>`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := serve(t, jsonHandler([]byte(body)))
			h := hybridFetcher(t, hybridSpec(writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli()), filepath.Join(t.TempDir(), "o.json")), srv)
			_, err := h.Fetch(context.Background())
			if ei := errInfo(t, err); ei.Code != CodeInvalidResponse {
				t.Fatalf("%+v", ei)
			}
		})
	}
	// a single valid window is enough; the missing one is not invented.
	srv := serve(t, jsonHandler([]byte(`{"seven_day":{"utilization":"12.5"}}`)))
	h := hybridFetcher(t, hybridSpec(writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli()), filepath.Join(t.TempDir(), "o.json")), srv)
	res, err := h.Fetch(context.Background())
	if err != nil || len(res.Quotas) != 1 || !approx(res.Quotas[0].UsedPercent, 12.5) || res.Quotas[0].Window.ResetAt != nil {
		t.Fatalf("%v %v", err, ids(res))
	}
}

func TestClaudeOAuthBodyLimitAndCancel(t *testing.T) {
	huge := serve(t, jsonHandler(bytes.Repeat([]byte(" "), maxBodyBytes+10)))
	h := hybridFetcher(t, hybridSpec(writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli()), filepath.Join(t.TempDir(), "o.json")), huge)
	if _, err := h.Fetch(context.Background()); errInfo(t, err).Code != CodeInvalidResponse {
		t.Fatal("oversized body accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := serve(t, jsonHandler([]byte(oauthUsageBody)))
	h = hybridFetcher(t, hybridSpec(writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli()), filepath.Join(t.TempDir(), "o.json")), srv)
	if _, err := h.Fetch(ctx); errInfo(t, err).Code != CodeCanceled || srv.hits.Load() != 0 {
		t.Fatal("canceled context must not reach the network")
	}
}

func TestClaudeHybridObserveIsNetworkFree(t *testing.T) {
	srv := serve(t, jsonHandler([]byte(oauthUsageBody)))
	obs := writeObs(t, fixture(t, "claude_observation.json"))
	obsBefore, _ := os.ReadFile(obs)
	creds := writeOAuthCreds(t, fakeNow.Add(-time.Hour).UnixMilli()) // expired: Observe must not care
	h := hybridFetcher(t, hybridSpec(creds, obs), srv)
	res, err := h.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if srv.hits.Load() != 0 {
		t.Fatalf("Observe made %d requests", srv.hits.Load())
	}
	if res.Source != SourceOpenCodeObserved || res.Identity != "" {
		t.Fatalf("source=%q identity=%q", res.Source, res.Identity)
	}
	// header utilization is a ratio: 0.38 -> 38 (x100), unchanged by the API path.
	if f := quotaByID(t, res, "five_hour"); !approx(f.UsedPercent, 38) {
		t.Fatalf("%+v", f)
	}
	if !res.ObservedAt.Equal(time.Date(2026, 11, 4, 9, 19, 49, 568e6, time.UTC)) {
		t.Fatalf("ObservedAt %v", res.ObservedAt)
	}
	after, _ := os.ReadFile(obs)
	if !bytes.Equal(obsBefore, after) {
		t.Fatal("observed file modified")
	}

	// failures keep their fixed category and carry no source claim.
	h = hybridFetcher(t, hybridSpec(creds, filepath.Join(t.TempDir(), "missing.json")), srv)
	res, err = h.Observe(context.Background())
	if errInfo(t, err).Code != CodeSourceUnavailable || res.Source != "" {
		t.Fatalf("%v %+v", err, res)
	}
	if srv.hits.Load() != 0 {
		t.Fatal("network used")
	}
}

func TestClaudeLegacyPassiveIsNotHybrid(t *testing.T) {
	file := writeObs(t, fixture(t, "claude_observation.json"))
	for _, s := range []Spec{
		claudeSpec(file),
		// credentials/pin fields without IdleAfter keep the plain passive collector.
		func() Spec {
			s := claudeSpec(file)
			s.CredentialFile = "/unused"
			s.ObserveInterval = time.Minute
			return s
		}(),
	} {
		c, err := New(s, opts(nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.(HybridCollector); ok {
			t.Fatalf("%T must not implement HybridCollector", c)
		}
		res, err := c.Fetch(context.Background())
		if err != nil || res.Source != "" || res.Identity != "" || len(res.Quotas) != 2 {
			t.Fatalf("%v %+v", err, res)
		}
	}
}

func TestClaudeHybridConstructorValidation(t *testing.T) {
	obs := filepath.Join(t.TempDir(), "o.json")
	creds := writeOAuthCreds(t, fakeNow.Add(time.Hour).UnixMilli())
	bad := map[string]Spec{
		"no credential file": hybridSpec("", obs),
		"no observed file":   hybridSpec(creds, ""),
		"negative idle": func() Spec {
			s := hybridSpec(creds, obs)
			s.ClaudeIdleAfter = -time.Second
			return s
		}(),
	}
	for name, s := range bad {
		if _, err := New(s, opts(nil)); err == nil || errInfo(t, err).Code != CodeConfigInvalid {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if _, err := New(hybridSpec(creds, obs), opts(map[string]string{"claude": "file:///etc/passwd"})); err == nil {
		t.Fatal("bad injected endpoint accepted")
	}
	if _, err := New(hybridSpec(creds, obs), Options{}); err != nil {
		t.Fatal(err)
	}
	// only Claude+observed becomes hybrid.
	s := hybridSpec(creds, obs)
	s.Definition.Provider = ProviderKimi
	if _, err := New(s, opts(nil)); err == nil {
		t.Fatal("kimi accepted")
	}
}

func TestClaudeHybridProductionEndpointFixed(t *testing.T) {
	c, err := New(hybridSpec("/c", "/o"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := c.(*claudeHybrid)
	if h.endpoint != "https://api.anthropic.com/api/oauth/usage" || strings.Contains(h.endpoint, "?") {
		t.Fatalf("endpoint %q", h.endpoint)
	}
}
