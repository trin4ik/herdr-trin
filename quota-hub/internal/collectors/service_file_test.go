package collectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// All values below are synthetic; the real registration is never read by tests.
const testServicePassword = "synthetic-service-password"

func basicFor(password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+password))
}

func writeRegistration(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func registrationFor(t *testing.T, url, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "service.json")
	writeRegistration(t, path, fmt.Sprintf(`{"url":%q,"pid":4242,"version":"2.2.5","password":%q}`, url, password))
	return path
}

func serviceSpec(provider, integration, file string) Spec {
	s := keyedSpec(provider, integration, "")
	s.OpenCodeServiceFile = file
	return s
}

type forbiddenTransport struct{ t *testing.T }

func (f forbiddenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("network request issued")
	return nil, fmt.Errorf("blocked")
}

func TestServiceFileUsesBasicAuthAndLeavesRegistrationUntouched(t *testing.T) {
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), basicFor(testServicePassword)))
	var gotAuth string
	prov := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		jsonHandler(fixture(t, "ollama_legacy.json"))(w, r)
	})
	file := registrationFor(t, oc.URL, testServicePassword)
	before, _ := os.ReadFile(file)
	infoBefore, _ := os.Stat(file)
	c, err := New(serviceSpec(ProviderOllama, "ollama-cloud", file), opts(map[string]string{"ollama": prov.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ollama-secret-key" || oc.hits.Load() != 1 || prov.hits.Load() != 1 {
		t.Fatalf("auth %q hits %d/%d", gotAuth, oc.hits.Load(), prov.hits.Load())
	}
	after, _ := os.ReadFile(file)
	infoAfter, _ := os.Stat(file)
	if !bytes.Equal(before, after) || !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatal("registration file was modified")
	}
}

func TestServiceFileChangesAreRereadEveryFetch(t *testing.T) {
	first := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), basicFor("first-password")))
	second := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), basicFor("second-password")))
	prov := serve(t, jsonHandler(fixture(t, "ollama_legacy.json")))
	file := registrationFor(t, first.URL, "first-password")
	c, err := New(serviceSpec(ProviderOllama, "ollama-cloud", file), opts(map[string]string{"ollama": prov.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeRegistration(t, file, fmt.Sprintf(`{"url":%q,"pid":4343,"version":"2.2.5","password":"second-password"}`, second.URL))
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.hits.Load() != 1 || second.hits.Load() != 1 {
		t.Fatalf("service restart was not picked up: %d/%d", first.hits.Load(), second.hits.Load())
	}
	// A rotated password on a new registration is sent, and a 401 is a plain auth error.
	oc := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != basicFor("wrong") {
			t.Error("stale password")
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	writeRegistration(t, file, fmt.Sprintf(`{"url":%q,"password":"wrong"}`, oc.URL))
	_, err = c.Fetch(context.Background())
	if ei := errInfo(t, err); ei.Code != CodeAuthRequired || ei.HTTPStatus != 401 || prov.hits.Load() != 2 {
		t.Fatalf("%+v", ei)
	}
}

func TestServiceFileWithoutPasswordSendsNoAuthorization(t *testing.T) {
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), ""))
	prov := serve(t, jsonHandler(fixture(t, "ollama_legacy.json")))
	file := filepath.Join(t.TempDir(), "service.json")
	writeRegistration(t, file, fmt.Sprintf(`{"url":%q,"pid":1,"version":"2.2.5"}`, oc.URL))
	c, _ := New(serviceSpec(ProviderOllama, "ollama-cloud", file), opts(map[string]string{"ollama": prov.URL}))
	if _, err := c.Fetch(context.Background()); err != nil || oc.hits.Load() != 1 {
		t.Fatalf("err=%v", err)
	}
}

func TestServiceFileExplicitEmptyPasswordIsStillBasic(t *testing.T) {
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), basicFor("")))
	prov := serve(t, jsonHandler(fixture(t, "ollama_legacy.json")))
	file := registrationFor(t, oc.URL, "")
	c, _ := New(serviceSpec(ProviderOllama, "ollama-cloud", file), opts(map[string]string{"ollama": prov.URL}))
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServiceFileKimiUsesSameAuth(t *testing.T) {
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), basicFor(testServicePassword)))
	prov := serve(t, jsonHandler(fixture(t, "kimi_live.json")))
	c, err := New(serviceSpec(ProviderKimi, "kimi-code-plan-cn", registrationFor(t, oc.URL, testServicePassword)), opts(map[string]string{"kimi": prov.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background()); err != nil || oc.hits.Load() != 1 {
		t.Fatalf("err=%v", err)
	}
}

func TestServiceFileUntrustedOriginsBlockedBeforeAnyNetworkOrPasswordUse(t *testing.T) {
	for _, bad := range []string{
		"https://example.com", "http://8.8.8.8:4096", "http://[2606:4700::1111]:4096",
		"http://localhost.evil.example:4096", "http://box", "http://user:pw@127.0.0.1:4096",
		"http://127.0.0.1:4096/api", "http://127.0.0.1:4096?x=1", "http://127.0.0.1:4096#f",
		"http://127.0.0.1:4096?", "ftp://127.0.0.1:4096", "http://0.0.0.0:4096", "http://127.0.0.1:0",
		"http://[fe80::1%25eth0]:4096", "127.0.0.1:4096", " http://127.0.0.1:4096",
	} {
		file := registrationFor(t, bad, testServicePassword)
		c, err := New(serviceSpec(ProviderOllama, "ollama-cloud", file),
			Options{HTTPClient: &http.Client{Transport: forbiddenTransport{t}}, Now: func() time.Time { return fakeNow }})
		if err != nil {
			t.Fatalf("%q: constructor must not read the registration: %v", bad, err)
		}
		_, err = c.Fetch(context.Background())
		ei := errInfo(t, err)
		if ei.Code != CodeConfigInvalid {
			t.Fatalf("%q accepted: %+v", bad, ei)
		}
		if msg := fmt.Sprintf("%v %+v %#v", err, ei, ei); strings.Contains(msg, testServicePassword) || strings.Contains(msg, file) || strings.Contains(msg, "8.8.8.8") {
			t.Fatalf("%q: error leaks registration data", bad)
		}
	}
	for _, good := range []string{"http://127.0.0.1:4096", "http://127.0.0.1:4096/", "https://192.168.50.10:443", "http://localhost:4096",
		"http://opencode.example.box:4096", "http://[::1]:4096", "http://[fd00::1]:4096"} {
		if _, ok := localOrigin(good); !ok {
			t.Fatalf("%q must be a trusted local origin", good)
		}
	}
}

func TestServiceFileMalformedRegistrationsFailClosed(t *testing.T) {
	dir := t.TempDir()
	huge := filepath.Join(dir, "huge.json")
	writeRegistration(t, huge, `{"url":"http://127.0.0.1:4096","password":"`+strings.Repeat("a", maxFileBytes)+`"}`)
	cases := map[string]string{
		"missing":      filepath.Join(dir, "absent.json"),
		"directory":    dir,
		"oversized":    huge,
		"not json":     "not json " + testServicePassword,
		"array":        `["http://127.0.0.1:4096"]`,
		"no url":       `{"password":"` + testServicePassword + `"}`,
		"url number":   `{"url":1,"password":"` + testServicePassword + `"}`,
		"empty object": `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			file := body
			if !strings.HasPrefix(body, "/") {
				file = filepath.Join(t.TempDir(), "service.json")
				writeRegistration(t, file, body)
			}
			c, err := New(serviceSpec(ProviderKimi, "kimi-code-plan-cn", file),
				Options{HTTPClient: &http.Client{Transport: forbiddenTransport{t}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Fetch(context.Background())
			ei := errInfo(t, err)
			if ei.Code != CodeCredentialsUnavailable {
				t.Fatalf("%+v", ei)
			}
			if msg := fmt.Sprintf("%v %+v", err, ei); strings.Contains(msg, testServicePassword) || strings.Contains(msg, file) {
				t.Fatal("error leaks registration data")
			}
		})
	}
	for name, password := range map[string]string{"number": `7`, "null": `null`, "object": `{}`} {
		file := filepath.Join(t.TempDir(), "service.json")
		writeRegistration(t, file, `{"url":"http://127.0.0.1:4096","password":`+password+`}`)
		c, _ := New(serviceSpec(ProviderKimi, "kimi-code-plan-cn", file), Options{HTTPClient: &http.Client{Transport: forbiddenTransport{t}}})
		if _, err := c.Fetch(context.Background()); errInfo(t, err).Code != CodeCredentialsUnavailable {
			t.Fatalf("password %s accepted", name)
		}
	}
}

func TestServiceFileRedirectDoesNotLeakPassword(t *testing.T) {
	other := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("service password leaked through a redirect")
		}
	})
	oc := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) })
	c, _ := New(serviceSpec(ProviderOllama, "ollama-cloud", registrationFor(t, oc.URL, testServicePassword)), opts(nil))
	_, err := c.Fetch(context.Background())
	if errInfo(t, err).Code != CodeCredentialsUnavailable || other.hits.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestServiceFileTransportNeverUsesProxy(t *testing.T) {
	c, err := New(serviceSpec(ProviderKimi, "kimi-code-plan-cn", "/nonexistent/service.json"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := c.(*kimiCollector).oc.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("service password request could transit an environment HTTP proxy")
	}
}

func TestServiceFileConstructorRules(t *testing.T) {
	file := "/nonexistent/opencode/service.json"
	if _, err := New(serviceSpec(ProviderOllama, "ollama-cloud", file), Options{}); err != nil {
		t.Fatalf("service_file without URL must construct without reading: %v", err)
	}
	bad := map[string]Spec{
		"with url":     serviceSpec(ProviderOllama, "ollama-cloud", file),
		"with bearer":  serviceSpec(ProviderOllama, "ollama-cloud", file),
		"relative":     serviceSpec(ProviderOllama, "ollama-cloud", "service.json"),
		"no integr.":   serviceSpec(ProviderOllama, "", file),
		"kimi + url":   serviceSpec(ProviderKimi, "kimi-code-plan-cn", file),
		"kimi bearer.": serviceSpec(ProviderKimi, "kimi-code-plan-cn", file),
	}
	withURL := bad["with url"]
	withURL.OpenCodeURL = "http://127.0.0.1:4096"
	bad["with url"] = withURL
	kimiURL := bad["kimi + url"]
	kimiURL.OpenCodeURL = "http://127.0.0.1:4096"
	bad["kimi + url"] = kimiURL
	for _, k := range []string{"with bearer", "kimi bearer."} {
		s := bad[k]
		s.OpenCodeBearerEnv = "OPENCODE_TOKEN"
		bad[k] = s
	}
	for name, s := range bad {
		if _, err := New(s, Options{}); err == nil || errInfo(t, err).Code != CodeConfigInvalid {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	// The test-only origin override is held to the same locality rule.
	for _, ep := range []string{"https://example.com", "http://8.8.8.8:1", "http://u:p@127.0.0.1:1", "http://127.0.0.1:1/x"} {
		if _, err := New(serviceSpec(ProviderOllama, "ollama-cloud", file), opts(map[string]string{"opencode": ep})); err == nil {
			t.Fatalf("override %q accepted", ep)
		}
	}
}

func TestServiceFileEndpointOverrideIgnoresRegistrationURL(t *testing.T) {
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), basicFor(testServicePassword)))
	prov := serve(t, jsonHandler(fixture(t, "ollama_legacy.json")))
	// The registration points to a port that nothing serves; the override wins.
	file := registrationFor(t, "http://127.0.0.1:1", testServicePassword)
	c, err := New(serviceSpec(ProviderOllama, "ollama-cloud", file), opts(map[string]string{"opencode": oc.URL, "ollama": prov.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background()); err != nil || oc.hits.Load() != 1 {
		t.Fatalf("err=%v", err)
	}
}

func TestStaticURLAndBearerStillWorkWithoutServiceFile(t *testing.T) {
	t.Setenv("QH_TEST_BEARER", "oc-token")
	oc := serve(t, openCodeHandler(t, fixture(t, "opencode_credentials.json"), "Bearer oc-token"))
	prov := serve(t, jsonHandler(fixture(t, "ollama_legacy.json")))
	spec := keyedSpec(ProviderOllama, "ollama-cloud", oc.URL)
	spec.OpenCodeBearerEnv = "QH_TEST_BEARER"
	c, err := New(spec, opts(map[string]string{"ollama": prov.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background()); err != nil || oc.hits.Load() != 1 {
		t.Fatalf("err=%v", err)
	}
}
