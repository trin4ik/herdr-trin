package collectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quota-hub/internal/model"
)

var fakeNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func opts(eps map[string]string) Options {
	return Options{Endpoints: eps, Now: func() time.Time { return fakeNow }}
}

func errInfo(t *testing.T, err error) *model.ErrorInfo {
	t.Helper()
	var ei *model.ErrorInfo
	if !errors.As(err, &ei) {
		t.Fatalf("want *model.ErrorInfo, got %T %v", err, err)
	}
	return ei
}

type counted struct {
	*httptest.Server
	hits atomic.Int32
}

func serve(t *testing.T, h http.HandlerFunc) *counted {
	t.Helper()
	c := &counted{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(c.Close)
	return c
}

func jsonHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}

func fakeJWT(exp int64) string {
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return "eyJhbGciOiJub25lIn0." + p + ".sig"
}

func writeAuth(t *testing.T, token, account string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens":         map[string]string{"access_token": token, "refresh_token": "never-used", "account_id": account},
	})
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func codexSpec(file string) Spec {
	return Spec{Definition: model.Definition{ID: "codex", Provider: ProviderCodex, Source: SourceNativeCodexOAuth}, CredentialFile: file}
}

func quotaByID(t *testing.T, r model.Result, id string) model.Quota {
	t.Helper()
	for _, q := range r.Quotas {
		if q.ID == id {
			return q
		}
	}
	t.Fatalf("quota %q not found in %+v", id, ids(r))
	return model.Quota{}
}

func ids(r model.Result) []string {
	var out []string
	for _, q := range r.Quotas {
		out = append(out, q.ID)
	}
	return out
}

func approx(p *float64, want float64) bool { return p != nil && *p > want-1e-6 && *p < want+1e-6 }

// --- Codex ---

func TestCodexSuccess(t *testing.T) {
	var gotAuth, gotAcct string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAcct = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id")
		jsonHandler(fixture(t, "codex_usage.json"))(w, r)
	})
	tok := fakeJWT(fakeNow.Add(time.Hour).Unix())
	file := writeAuth(t, tok, "acct-test")
	before, _ := os.ReadFile(file)
	c, err := New(codexSpec(file), opts(map[string]string{"codex": srv.URL}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+tok || gotAcct != "acct-test" {
		t.Fatalf("headers %q %q", gotAuth, gotAcct)
	}
	if len(res.Quotas) != 1 {
		t.Fatalf("want only weekly, got %v", ids(res))
	}
	q := quotaByID(t, res, "weekly")
	if !approx(q.UsedPercent, 32) || !approx(q.RemainingPercent, 68) || *q.Window.DurationSeconds != 604800 || *q.Window.Period != "week" {
		t.Fatalf("%+v", q)
	}
	if q.Window.ResetAt == nil || q.Window.ResetAt.Unix() != 1794343662 || q.Window.StartedAt != nil {
		t.Fatalf("window %+v", q.Window)
	}
	if q.Used != nil || q.Limit != nil || q.Remaining != nil {
		t.Fatal("absolute values must stay null")
	}
	if res.Plan == nil || *res.Plan != "prolite" || !res.ObservedAt.Equal(fakeNow) || len(res.Raw) == 0 {
		t.Fatalf("meta %+v", res)
	}
	if len(res.Metrics) != 1 || res.Metrics[0].Value != 12.5 {
		t.Fatalf("metrics %+v", res.Metrics)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("credential file modified")
	}
	if srv.hits.Load() != 1 {
		t.Fatalf("hits %d", srv.hits.Load())
	}
}

func TestCodexWindowsByDuration(t *testing.T) {
	srv := serve(t, jsonHandler(fixture(t, "codex_multi.json")))
	c, _ := New(codexSpec(writeAuth(t, "opaque-token", "a")), opts(map[string]string{"codex": srv.URL}))
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w := quotaByID(t, res, "weekly")
	if !approx(w.UsedPercent, 10.5) || w.Window.ResetAt == nil || !w.Window.ResetAt.Equal(fakeNow.Add(time.Hour)) {
		t.Fatalf("weekly %+v", w)
	}
	// secondary with 5h duration is the 5h window; position does not decide.
	if f := quotaByID(t, res, "five_hour"); !approx(f.UsedPercent, 20) {
		t.Fatalf("%+v", f)
	}
	cr := quotaByID(t, res, "code_review_weekly")
	if cr.Scope == nil || *cr.Scope != "code_review" {
		t.Fatalf("%+v", cr)
	}
	ad := quotaByID(t, res, "gpt_6_astra_five_hour")
	if ad.Scope == nil || *ad.Scope != "GPT-6 Astra" || !approx(ad.UsedPercent, 50) {
		t.Fatalf("%+v", ad)
	}
	if res.Plan == nil || *res.Plan != "pro" || len(res.Metrics) != 0 {
		t.Fatal("plan/metrics")
	}
}

func TestCodexNoRequestOnBadCredentials(t *testing.T) {
	srv := serve(t, jsonHandler(fixture(t, "codex_usage.json")))
	ep := map[string]string{"codex": srv.URL}
	big := filepath.Join(t.TempDir(), "big.json")
	os.WriteFile(big, bytes.Repeat([]byte(" "), maxFileBytes+10), 0o600)
	garbage := filepath.Join(t.TempDir(), "garbage.json")
	os.WriteFile(garbage, []byte("not json"), 0o600)
	cases := map[string]struct {
		file string
		code string
	}{
		"missing":     {filepath.Join(t.TempDir(), "none.json"), CodeCredentialsUnavailable},
		"no account":  {writeAuth(t, "tok", ""), CodeCredentialsUnavailable},
		"no token":    {writeAuth(t, "", "acct"), CodeCredentialsUnavailable},
		"space token": {writeAuth(t, "to ken", "acct"), CodeCredentialsUnavailable},
		"oversized":   {big, CodeCredentialsUnavailable},
		"garbage":     {garbage, CodeCredentialsUnavailable},
		"directory":   {t.TempDir(), CodeCredentialsUnavailable},
		"jwt expired": {writeAuth(t, fakeJWT(fakeNow.Add(-time.Hour).Unix()), "acct"), CodeAuthRequired},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := New(codexSpec(tc.file), opts(ep))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Fetch(context.Background())
			if ei := errInfo(t, err); ei.Code != tc.code {
				t.Fatalf("got %q", ei.Code)
			}
		})
	}
	if srv.hits.Load() != 0 {
		t.Fatalf("requests with bad credentials: %d", srv.hits.Load())
	}
}

func TestCodexRecentlyExpiredNeverTries(t *testing.T) {
	srv := serve(t, jsonHandler(fixture(t, "codex_usage.json")))
	c, _ := New(codexSpec(writeAuth(t, fakeJWT(fakeNow.Add(-10*time.Second).Unix()), "a")), opts(map[string]string{"codex": srv.URL}))
	if _, err := c.Fetch(context.Background()); err == nil || errInfo(t, err).Code != CodeAuthRequired || srv.hits.Load() != 0 {
		t.Fatalf("err=%v hits=%d", err, srv.hits.Load())
	}
}

// --- shared HTTP behaviour (via Codex) ---

func TestStatusMapping(t *testing.T) {
	retryDate := fakeNow.Add(48 * time.Hour).UTC()
	cases := []struct {
		name   string
		status int
		header map[string]string
		code   string
		retry  *time.Time
	}{
		{"401", 401, nil, CodeAuthRequired, nil},
		{"403", 403, nil, CodeAccessDenied, nil},
		{"429 seconds", 429, map[string]string{"Retry-After": "120"}, CodeRateLimited, tptr(fakeNow.Add(2 * time.Minute))},
		{"429 long date preserved", 429, map[string]string{"Retry-After": retryDate.Format(http.TimeFormat)}, CodeRateLimited, &retryDate},
		{"429 none", 429, nil, CodeRateLimited, nil},
		{"429 past date", 429, map[string]string{"Retry-After": fakeNow.Add(-time.Hour).Format(http.TimeFormat)}, CodeRateLimited, nil},
		{"429 junk", 429, map[string]string{"Retry-After": "soon"}, CodeRateLimited, nil},
		{"500", 500, nil, CodeUpstreamError, nil},
		{"503", 503, nil, CodeUpstreamError, nil},
		{"404", 404, nil, CodeInvalidResponse, nil},
		{"204", 204, nil, CodeInvalidResponse, nil},
		{"302", 302, map[string]string{"Location": "http://127.0.0.1:1/"}, CodeInvalidResponse, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"secret":"body-must-not-leak"}`))
			})
			c, _ := New(codexSpec(writeAuth(t, "tok", "a")), opts(map[string]string{"codex": srv.URL}))
			_, err := c.Fetch(context.Background())
			ei := errInfo(t, err)
			if ei.Code != tc.code || ei.HTTPStatus != tc.status {
				t.Fatalf("got %+v", ei)
			}
			if (ei.RetryAt == nil) != (tc.retry == nil) || (tc.retry != nil && !ei.RetryAt.Equal(*tc.retry)) {
				t.Fatalf("retry_at %v want %v", ei.RetryAt, tc.retry)
			}
			if strings.Contains(err.Error(), "body-must-not-leak") || strings.Contains(err.Error(), srv.URL) {
				t.Fatal("error leaks")
			}
			if srv.hits.Load() != 1 {
				t.Fatalf("hits %d (no retries)", srv.hits.Load())
			}
		})
	}
}

func TestRedirectNotFollowedAndTokenNotLeaked(t *testing.T) {
	other := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("authorization leaked on redirect")
		}
		w.Write([]byte(`{}`))
	})
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	})
	// A permissive injected client must be overridden.
	o := opts(map[string]string{"codex": srv.URL})
	o.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	c, _ := New(codexSpec(writeAuth(t, "tok", "a")), o)
	_, err := c.Fetch(context.Background())
	if ei := errInfo(t, err); ei.Code != CodeInvalidResponse || ei.HTTPStatus != 307 {
		t.Fatalf("%+v", ei)
	}
	if other.hits.Load() != 0 {
		t.Fatal("redirect followed")
	}
	if o.HTTPClient.CheckRedirect == nil {
		t.Fatal("caller client mutated")
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	release := make(chan struct{})
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	spec := codexSpec(writeAuth(t, "tok", "a"))
	spec.Timeout = 50 * time.Millisecond
	c, _ := New(spec, opts(map[string]string{"codex": srv.URL}))
	_, err := c.Fetch(context.Background())
	if ei := errInfo(t, err); ei.Code != CodeTimeout {
		t.Fatalf("%+v", ei)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Fetch(ctx)
	if ei := errInfo(t, err); ei.Code != CodeCanceled {
		t.Fatalf("%+v", ei)
	}
}

func TestBodyLimitsAndMalformed(t *testing.T) {
	big := append([]byte(`{"pad":"`), bytes.Repeat([]byte("x"), maxBodyBytes)...)
	big = append(big, []byte(`"}`)...)
	cases := map[string][]byte{
		"oversized":    big,
		"not json":     []byte("<html>"),
		"empty object": []byte(`{}`),
		"array":        []byte(`[]`),
		"bad window":   []byte(`{"rate_limit":{"primary_window":{"used_percent":"lots"}}}`),
		"null window":  []byte(`{"rate_limit":{"primary_window":{"used_percent":null}}}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := serve(t, jsonHandler(body))
			c, _ := New(codexSpec(writeAuth(t, "tok", "a")), opts(map[string]string{"codex": srv.URL}))
			_, err := c.Fetch(context.Background())
			if ei := errInfo(t, err); ei.Code != CodeInvalidResponse {
				t.Fatalf("%+v", ei)
			}
		})
	}
}

func TestOversizedErrorBodyStillMapsStatus(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write(bytes.Repeat([]byte("x"), 2*maxBodyBytes))
	})
	c, _ := New(codexSpec(writeAuth(t, "tok", "a")), opts(map[string]string{"codex": srv.URL}))
	_, err := c.Fetch(context.Background())
	if ei := errInfo(t, err); ei.Code != CodeAuthRequired {
		t.Fatalf("%+v", ei)
	}
}

// --- OpenCode credential source ---

func openCodeHandler(t *testing.T, body []byte, wantBearer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/credential" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != wantBearer {
			t.Errorf("opencode auth %q want %q", got, wantBearer)
		}
		w.Write(body)
	}
}

func keyedSpec(provider, integration string, ocURL string) Spec {
	return Spec{
		Definition:    model.Definition{ID: provider, Provider: provider, Source: SourceOpenCodeKey},
		IntegrationID: integration,
		OpenCodeURL:   ocURL,
	}
}

func TestOllamaViaOpenCodeKeys(t *testing.T) {
	t.Setenv("QH_TEST_BEARER", "oc-token")
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), "Bearer oc-token"))
	var gotAuth string
	prov := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		jsonHandler(fixture(t, "ollama_legacy.json"))(w, r)
	})
	spec := keyedSpec(ProviderOllama, "ollama-cloud", oc.URL)
	spec.OpenCodeBearerEnv = "QH_TEST_BEARER"
	c, err := New(spec, opts(map[string]string{"ollama": prov.URL}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ollama-secret-key" {
		t.Fatalf("auth %q", gotAuth)
	}
	s := quotaByID(t, res, "session")
	if !approx(s.RemainingPercent, 100) || s.Window.DurationSeconds != nil || s.Window.Period != nil || s.Window.StartedAt != nil ||
		s.Window.ResetAt == nil || !s.Window.ResetAt.Equal(time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("session %+v", s)
	}
	w := quotaByID(t, res, "weekly")
	if !approx(w.RemainingPercent, 99.46) || !approx(w.UsedPercent, 0.54) || *w.Window.Period != "week" {
		t.Fatalf("weekly %+v", w)
	}
	if len(res.Quotas) != 2 || len(res.Metrics) != 1 || res.Metrics[0].ID != "purchased_balance" || res.Metrics[0].Value != 0 || res.Metrics[0].Unit != "USD" {
		t.Fatalf("%+v %+v", res.Quotas, res.Metrics)
	}
	if oc.hits.Load() != 1 || prov.hits.Load() != 1 {
		t.Fatal("one request each")
	}
}

func TestOllamaCreditPlan(t *testing.T) {
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), ""))
	prov := serve(t, jsonHandler(fixture(t, "ollama_credit.json")))
	c, _ := New(keyedSpec(ProviderOllama, "ollama-cloud", oc.URL), opts(map[string]string{"ollama": prov.URL}))
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Quotas) != 1 {
		t.Fatalf("%v", ids(res))
	}
	m := quotaByID(t, res, "monthly")
	if !approx(m.RemainingPercent, 75) || !approx(m.UsedPercent, 25) || *m.Window.Period != "month" ||
		m.Window.StartedAt == nil || m.Window.ResetAt == nil || *m.Window.DurationSeconds != 30*86400 {
		t.Fatalf("%+v", m)
	}
	if len(res.Metrics) != 1 || res.Metrics[0].Value != 3.25 {
		t.Fatalf("%+v", res.Metrics)
	}
}

func TestOllamaCreditZeroAllowanceNotQuota(t *testing.T) {
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), ""))
	prov := serve(t, jsonHandler([]byte(`{"included":{"balance_usd":0,"allowance_usd":0},"purchased":{"balance_usd":1}}`)))
	c, _ := New(keyedSpec(ProviderOllama, "ollama-cloud", oc.URL), opts(map[string]string{"ollama": prov.URL}))
	res, err := c.Fetch(context.Background())
	if err != nil || len(res.Quotas) != 0 || len(res.Metrics) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestCredentialSelection(t *testing.T) {
	mk := func(entries string) []byte { return []byte(`{"data":[` + entries + `]}`) }
	key := func(id, integ string, active bool, k string) string {
		return fmt.Sprintf(`{"id":%q,"integrationID":%q,"active":%v,"value":{"type":"key","key":%q}}`, id, integ, active, k)
	}
	cases := []struct {
		name    string
		body    []byte
		credID  string
		wantKey string
		fail    bool
	}{
		{"active only", mk(key("a", "ollama-cloud", false, "old") + "," + key("b", "ollama-cloud", true, "new")), "", "new", false},
		{"ambiguous active", mk(key("a", "ollama-cloud", true, "k1") + "," + key("b", "ollama-cloud", true, "k2")), "", "", true},
		{"none active", mk(key("a", "ollama-cloud", false, "k1")), "", "", true},
		{"other integration only", mk(key("a", "kimi-code-plan-cn", true, "k1")), "", "", true},
		{"pinned id", mk(key("a", "ollama-cloud", true, "k1") + "," + key("b", "ollama-cloud", false, "k2")), "b", "k2", false},
		{"pinned wrong integration", mk(key("a", "kimi-code-plan-cn", true, "k1")), "a", "", true},
		{"pinned missing", mk(key("a", "ollama-cloud", true, "k1")), "zzz", "", true},
		{"oauth refused", mk(`{"id":"a","integrationID":"ollama-cloud","active":true,"value":{"type":"oauth","access":"x","refresh":"y","expires":1}}`), "", "", true},
		{"external refused", mk(`{"id":"a","integrationID":"ollama-cloud","active":true,"value":{"type":"external","methodID":"m"}}`), "", "", true},
		{"empty key", mk(key("a", "ollama-cloud", true, "")), "", "", true},
		{"garbage", []byte("nope"), "", "", true},
		{"bare array", []byte("[" + key("a", "ollama-cloud", true, "arr") + "]"), "", "arr", false},
		{"unrelated malformed entry ignored", mk(`"junk",` + key("a", "ollama-cloud", true, "k1")), "", "k1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oc := serve(t, openCodeHandler(t, tc.body, ""))
			var gotAuth string
			prov := serve(t, func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				jsonHandler(fixture(t, "ollama_legacy.json"))(w, r)
			})
			spec := keyedSpec(ProviderOllama, "ollama-cloud", oc.URL)
			spec.CredentialID = tc.credID
			c, _ := New(spec, opts(map[string]string{"ollama": prov.URL}))
			_, err := c.Fetch(context.Background())
			if tc.fail {
				if ei := errInfo(t, err); ei.Code != CodeCredentialsUnavailable {
					t.Fatalf("%+v", ei)
				}
				if prov.hits.Load() != 0 {
					t.Fatal("provider request despite bad credentials")
				}
				return
			}
			if err != nil || gotAuth != "Bearer "+tc.wantKey {
				t.Fatalf("err=%v auth=%q", err, gotAuth)
			}
		})
	}
}

func TestOpenCodeAccessFailures(t *testing.T) {
	t.Setenv("QH_EMPTY", "")
	for _, st := range []int{401, 403, 500} {
		oc := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(st) })
		prov := serve(t, jsonHandler(fixture(t, "ollama_legacy.json")))
		c, _ := New(keyedSpec(ProviderOllama, "ollama-cloud", oc.URL), opts(map[string]string{"ollama": prov.URL}))
		_, err := c.Fetch(context.Background())
		wantCode := map[int]string{401: CodeAuthRequired, 403: CodeAccessDenied, 500: CodeUpstreamError}[st]
		if ei := errInfo(t, err); ei.Code != wantCode || ei.HTTPStatus != st {
			t.Fatalf("%d: %+v", st, ei)
		}
		if prov.hits.Load() != 0 {
			t.Fatal("provider requested")
		}
	}
	// configured bearer env that is empty: fail locally, no request at all.
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), ""))
	spec := keyedSpec(ProviderOllama, "ollama-cloud", oc.URL)
	spec.OpenCodeBearerEnv = "QH_EMPTY"
	c, _ := New(spec, opts(nil))
	if _, err := c.Fetch(context.Background()); errInfo(t, err).Code != CodeCredentialsUnavailable || oc.hits.Load() != 0 {
		t.Fatal("empty bearer env must not request")
	}
	// oversized credential list.
	huge := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write(bytes.Repeat([]byte(" "), maxCredBytes+5)) })
	c, _ = New(keyedSpec(ProviderOllama, "ollama-cloud", huge.URL), opts(nil))
	if _, err := c.Fetch(context.Background()); errInfo(t, err).Code != CodeCredentialsUnavailable {
		t.Fatal("oversize creds")
	}
}

func TestOpenCodeRedirectRejected(t *testing.T) {
	other := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer leaked")
		}
	})
	oc := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) })
	t.Setenv("QH_TEST_BEARER", "oc-token")
	spec := keyedSpec(ProviderOllama, "ollama-cloud", oc.URL)
	spec.OpenCodeBearerEnv = "QH_TEST_BEARER"
	c, _ := New(spec, opts(nil))
	if _, err := c.Fetch(context.Background()); errInfo(t, err).Code != CodeCredentialsUnavailable || other.hits.Load() != 0 {
		t.Fatal("redirect")
	}
}

func TestProviderRedirectDoesNotLeakKey(t *testing.T) {
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), ""))
	other := serve(t, func(w http.ResponseWriter, r *http.Request) { t.Error("followed") })
	prov := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) })
	c, _ := New(keyedSpec(ProviderKimi, "kimi-code-plan-cn", oc.URL), opts(map[string]string{"kimi": prov.URL}))
	if _, err := c.Fetch(context.Background()); errInfo(t, err).Code != CodeInvalidResponse || other.hits.Load() != 0 {
		t.Fatal("redirect")
	}
}

// --- Kimi ---

func kimiFetch(t *testing.T, fx string) model.Result {
	t.Helper()
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), ""))
	var gotAuth string
	prov := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		jsonHandler(fixture(t, fx))(w, r)
	})
	c, err := New(keyedSpec(ProviderKimi, "kimi-code-plan-cn", oc.URL), opts(map[string]string{"kimi": prov.URL}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer kimi-secret-key" {
		t.Fatalf("auth %q", gotAuth)
	}
	return res
}

func TestKimiLiveMonthly(t *testing.T) {
	res := kimiFetch(t, "kimi_live.json")
	if len(res.Quotas) != 3 {
		t.Fatalf("limits[] must not duplicate usages: %v", ids(res))
	}
	f := quotaByID(t, res, "limit_5h")
	if !approx(f.UsedPercent, 0) || !approx(f.RemainingPercent, 100) || *f.Window.DurationSeconds != 18000 ||
		!f.Window.ResetAt.Equal(time.Date(2026, 11, 4, 11, 52, 10, 0, time.UTC)) || f.Window.StartedAt != nil {
		t.Fatalf("%+v", f)
	}
	for id, scope := range map[string]string{"limit_month_total": "total", "limit_month_code": "code"} {
		q := quotaByID(t, res, id)
		if !approx(q.UsedPercent, 11.25) || !approx(q.RemainingPercent, 88.75) || *q.Window.Period != "month" ||
			q.Window.DurationSeconds != nil || *q.Scope != scope ||
			!q.Window.ResetAt.Equal(time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("%s %+v", id, q)
		}
	}
	for _, q := range res.Quotas {
		if q.ID == "limit_weekly" || (q.Window.Period != nil && *q.Window.Period == "week") {
			t.Fatal("invented weekly")
		}
	}
	if len(res.Metrics) != 0 || bytes.Contains(mustJSON(res.Quotas), []byte("wallet")) {
		t.Fatal("booster wallet leaked into normalized output")
	}
	if !bytes.Contains(res.Raw, []byte("booster_wallet")) {
		t.Fatal("raw must be preserved privately")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestKimiLimitsFallbackDedup(t *testing.T) {
	res := kimiFetch(t, "kimi_limits_only.json")
	if len(res.Quotas) != 3 {
		t.Fatalf("%v", ids(res))
	}
	f := quotaByID(t, res, "limit_5h")
	if !approx(f.UsedPercent, 60) || *f.Window.DurationSeconds != 18000 {
		t.Fatalf("first duplicate wins: %+v", f)
	}
	m := quotaByID(t, res, "limit_month")
	if *m.Window.Period != "month" || m.Window.DurationSeconds != nil || !approx(m.UsedPercent, 10) {
		t.Fatalf("%+v", m)
	}
	w := quotaByID(t, res, "limit_weekly")
	if *w.Window.Period != "week" || !approx(w.UsedPercent, 25) {
		t.Fatalf("%+v", w)
	}
	for _, q := range res.Quotas {
		if q.Unit != "percent" || q.Used != nil || q.Limit != nil {
			t.Fatalf("100/100 detail must not become request counts: %+v", q)
		}
	}
}

func TestKimiUnknownKeysGeneric(t *testing.T) {
	res := kimiFetch(t, "kimi_unknown_key.json")
	if len(res.Quotas) != 2 {
		t.Fatalf("%v", ids(res))
	}
	e := quotaByID(t, res, "limit_extra")
	if !approx(e.UsedPercent, 50) || e.Window.Period != nil || e.Window.DurationSeconds != nil || e.Window.StartedAt != nil || e.Window.ResetAt != nil {
		t.Fatalf("%+v", e)
	}
}

func TestKimiEndpointSelection(t *testing.T) {
	for integ, want := range map[string]string{
		"kimi-code-plan-cn":     EndpointKimiCN,
		"kimi-code-plan-global": EndpointKimiGlob,
		"anything-else":         EndpointKimiCN,
	} {
		c, err := New(keyedSpec(ProviderKimi, integ, "http://127.0.0.1:1"), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.(*kimiCollector).endpoint; got != want {
			t.Fatalf("%s: %s", integ, got)
		}
	}
	c, _ := New(codexSpec("/x"), Options{})
	if c.(*codexCollector).endpoint != EndpointCodex {
		t.Fatal("codex endpoint")
	}
	c, _ = New(keyedSpec(ProviderOllama, "ollama-cloud", "http://127.0.0.1:1"), Options{})
	if c.(*ollamaCollector).endpoint != EndpointOllama {
		t.Fatal("ollama endpoint")
	}
}

// --- Claude observed file ---

func claudeSpec(file string) Spec {
	return Spec{Definition: model.Definition{ID: "claude", Provider: ProviderClaude, Source: SourceOpenCodeObserved}, ObservedFile: file}
}

func writeObs(t *testing.T, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func obsJSON(observedAt string, status int, quota string) []byte {
	return []byte(fmt.Sprintf(`{"schema_version":1,"source":"opencode-http-response-hook","observed_at":%q,"provider_id":"anthropic","status":%d,"quota":[%s]}`, observedAt, status, quota))
}

func TestClaudeObserved(t *testing.T) {
	file := writeObs(t, fixture(t, "claude_observation.json"))
	before, _ := os.ReadFile(file)
	// time moves on: ObservedAt must still be the file's.
	o := opts(nil)
	later := fakeNow.Add(72 * time.Hour)
	o.Now = func() time.Time { return later }
	c, err := New(claudeSpec(file), o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ObservedAt.Equal(time.Date(2026, 11, 4, 9, 19, 49, 568e6, time.UTC)) {
		t.Fatalf("ObservedAt %v", res.ObservedAt)
	}
	f := quotaByID(t, res, "five_hour")
	if !approx(f.UsedPercent, 38) || !approx(f.RemainingPercent, 62) || *f.Window.DurationSeconds != 18000 ||
		!f.Window.ResetAt.Equal(time.Date(2026, 11, 4, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("%+v", f)
	}
	s := quotaByID(t, res, "seven_day")
	if !approx(s.UsedPercent, 50) || *s.Window.DurationSeconds != 604800 {
		t.Fatalf("%+v", s)
	}
	if len(res.Quotas) != 2 || len(res.Metrics) != 0 {
		t.Fatalf("%v", ids(res))
	}
	if bytes.Contains(res.Raw, []byte("do-not-keep")) || bytes.Contains(res.Raw, []byte("x-unrelated")) || !bytes.Contains(res.Raw, []byte("5h-utilization")) {
		t.Fatalf("raw not sanitized: %s", res.Raw)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("observed file modified")
	}
}

func TestClaude429ExhaustedIsData(t *testing.T) {
	q := `{"name":"anthropic-ratelimit-unified-5h-utilization","type":"number","value":1},
	      {"name":"anthropic-ratelimit-unified-5h-reset","type":"time","value":"2026-11-04T14:00:00Z"}`
	c, _ := New(claudeSpec(writeObs(t, obsJSON("2026-11-04T11:59:00Z", 429, q))), opts(nil))
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := quotaByID(t, res, "five_hour")
	if !approx(f.UsedPercent, 100) || !approx(f.RemainingPercent, 0) {
		t.Fatalf("%+v", f)
	}
	// 7d absent: not invented.
	if len(res.Quotas) != 1 {
		t.Fatalf("%v", ids(res))
	}
	var raw observation
	if json.Unmarshal(res.Raw, &raw) != nil || raw.Status != 429 {
		t.Fatalf("raw %s", res.Raw)
	}
}

func TestClaudeStatusesAndOptionalWindows(t *testing.T) {
	q := `{"name":"anthropic-ratelimit-unified-7d-utilization","type":"number","value":0.25}`
	for _, st := range []int{200, 201, 429} {
		c, _ := New(claudeSpec(writeObs(t, obsJSON("2026-11-04T11:00:00Z", st, q))), opts(nil))
		res, err := c.Fetch(context.Background())
		if err != nil || len(res.Quotas) != 1 {
			t.Fatalf("%d: %v", st, err)
		}
		s := quotaByID(t, res, "seven_day")
		if s.Window.ResetAt != nil || !approx(s.UsedPercent, 25) {
			t.Fatalf("%+v", s)
		}
	}
	for _, st := range []int{0, 400, 401, 500, 302} {
		c, _ := New(claudeSpec(writeObs(t, obsJSON("2026-11-04T11:00:00Z", st, q))), opts(nil))
		if _, err := c.Fetch(context.Background()); errInfo(t, err).Code != CodeInvalidResponse {
			t.Fatalf("status %d accepted", st)
		}
	}
}

func TestClaudeModelScopedWeekly(t *testing.T) {
	q := `{"name":"anthropic-ratelimit-unified-7d-utilization","type":"number","value":0.1},
	      {"name":"anthropic-ratelimit-unified-7d-opus-utilization","type":"number","value":0.6},
	      {"name":"anthropic-ratelimit-unified-7d_sonnet-utilization","type":"number","value":"0.7"},
	      {"name":"anthropic-ratelimit-unified-7d-opus-reset","type":"time","value":"2026-11-06T07:00:00Z"},
	      {"name":"anthropic-ratelimit-unified-7d-opus-status","type":"enum","value":"allowed_warning"}`
	c, _ := New(claudeSpec(writeObs(t, obsJSON("2026-11-04T11:00:00Z", 200, q))), opts(nil))
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := quotaByID(t, res, "seven_day_opus")
	if *o.Scope != "opus" || !approx(o.UsedPercent, 60) || *o.Window.DurationSeconds != 604800 || o.Window.ResetAt == nil {
		t.Fatalf("%+v", o)
	}
	if s := quotaByID(t, res, "seven_day_sonnet"); *s.Scope != "sonnet" || !approx(s.UsedPercent, 70) {
		t.Fatalf("%+v", s)
	}
	if g := quotaByID(t, res, "seven_day"); g.Scope != nil {
		t.Fatalf("%+v", g)
	}
}

func TestClaudeRejects(t *testing.T) {
	good := `{"name":"anthropic-ratelimit-unified-5h-utilization","type":"number","value":0.5}`
	future := fakeNow.Add(10 * time.Minute).Format(time.RFC3339)
	cases := map[string][]byte{
		"future":         obsJSON(future, 200, good),
		"no quota":       obsJSON("2026-11-04T11:00:00Z", 200, ""),
		"only reset":     obsJSON("2026-11-04T11:00:00Z", 200, `{"name":"anthropic-ratelimit-unified-5h-reset","type":"time","value":"2026-11-04T14:00:00Z"}`),
		"only status":    obsJSON("2026-11-04T11:00:00Z", 200, `{"name":"anthropic-ratelimit-unified-5h-status","type":"enum","value":"allowed"}`),
		"wrong type":     obsJSON("2026-11-04T11:00:00Z", 200, `{"name":"anthropic-ratelimit-unified-5h-utilization","type":"enum","value":"x"}`),
		"negative":       obsJSON("2026-11-04T11:00:00Z", 200, `{"name":"anthropic-ratelimit-unified-5h-utilization","type":"number","value":-1}`),
		"null value":     obsJSON("2026-11-04T11:00:00Z", 200, `{"name":"anthropic-ratelimit-unified-5h-utilization","type":"number","value":null}`),
		"bad time":       obsJSON("yesterday", 200, good),
		"wrong provider": bytes.ReplaceAll(obsJSON("2026-11-04T11:00:00Z", 200, good), []byte("anthropic\""), []byte("openai\"")),
		"wrong schema":   bytes.ReplaceAll(obsJSON("2026-11-04T11:00:00Z", 200, good), []byte(`"schema_version":1`), []byte(`"schema_version":2`)),
		"wrong source":   bytes.ReplaceAll(obsJSON("2026-11-04T11:00:00Z", 200, good), []byte("opencode-http-response-hook"), []byte("other")),
		"wrapper array":  []byte(`{"schema_version":1,"observations":[]}`),
		"not json":       []byte("???"),
		"oversized":      bytes.Repeat([]byte(" "), maxObsBytes+1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := New(claudeSpec(writeObs(t, body)), opts(nil))
			_, err := c.Fetch(context.Background())
			if ei := errInfo(t, err); ei.Code != CodeInvalidResponse && ei.Code != CodeSourceUnavailable {
				t.Fatalf("%+v", ei)
			}
		})
	}
	// within allowed skew is accepted.
	ok := fakeNow.Add(2 * time.Minute).Format(time.RFC3339)
	c, _ := New(claudeSpec(writeObs(t, obsJSON(ok, 200, good))), opts(nil))
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, _ = New(claudeSpec(filepath.Join(t.TempDir(), "missing.json")), opts(nil))
	if _, err := c.Fetch(context.Background()); errInfo(t, err).Code != CodeSourceUnavailable {
		t.Fatal("missing file")
	}
}

// --- constructor ---

func TestNewValidation(t *testing.T) {
	def := func(p, s string) model.Definition { return model.Definition{ID: "x", Provider: p, Source: s} }
	bad := []Spec{
		{Definition: def("nope", SourceOpenCodeKey)},
		{Definition: def(ProviderCodex, SourceOpenCodeKey), CredentialFile: "/x"},
		{Definition: def(ProviderCodex, SourceNativeCodexOAuth)},
		{Definition: def(ProviderClaude, SourceOpenCodeObserved)},
		{Definition: def(ProviderOllama, SourceOpenCodeKey), OpenCodeURL: "http://127.0.0.1:1"},
		{Definition: def(ProviderOllama, SourceOpenCodeKey), IntegrationID: "ollama-cloud"},
		{Definition: def(ProviderOllama, SourceOpenCodeKey), IntegrationID: "ollama-cloud", OpenCodeURL: "ftp://x"},
		{Definition: def(ProviderOllama, SourceOpenCodeKey), IntegrationID: "ollama-cloud", OpenCodeURL: "http://user:pw@127.0.0.1:1"},
		{Definition: def(ProviderKimi, SourceOpenCodeObserved), ObservedFile: "/x"},
	}
	for i, s := range bad {
		if _, err := New(s, Options{}); err == nil || errInfo(t, err).Code != CodeConfigInvalid {
			t.Fatalf("case %d accepted: %v", i, err)
		}
	}
	if _, err := New(codexSpec("/x"), Options{Endpoints: map[string]string{"codex": "file:///etc/passwd"}}); err == nil {
		t.Fatal("bad injected endpoint accepted")
	}
}

func TestNeverLogsOrReturnsSecrets(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	tok := "super-secret-token"
	c, _ := New(codexSpec(writeAuth(t, tok, "acct-secret")), opts(map[string]string{"codex": srv.URL}))
	_, err := c.Fetch(context.Background())
	msg := fmt.Sprintf("%v %+v %#v", err, err, err)
	for _, s := range []string{tok, "acct-secret", srv.URL, "auth.json"} {
		if strings.Contains(msg, s) {
			t.Fatalf("error leaks %q", s)
		}
	}
}
