package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quota-hub/internal/model"
)

type absent struct{}

type profCase struct {
	name, provider, integration, endpointKey, method, path string
	build                                                  func(id, email, plan any) map[string]any
}

func put(m map[string]any, k string, v any) {
	if _, skip := v.(absent); !skip {
		m[k] = v
	}
}

var profCases = []profCase{
	{"ollama", ProviderOllama, "ollama-cloud", "ollama_profile", http.MethodPost, "/api/me", func(id, email, plan any) map[string]any {
		m := map[string]any{}
		put(m, "ID", id)
		put(m, "Email", email)
		put(m, "Plan", plan)
		return m
	}},
	{"ollama lowercase", ProviderOllama, "ollama-cloud", "ollama_profile", http.MethodPost, "/api/me", func(id, email, plan any) map[string]any {
		m := map[string]any{}
		put(m, "id", id)
		put(m, "email", email)
		put(m, "plan", plan)
		return m
	}},
	{"kimi cn", ProviderKimi, "kimi-code-plan-cn", "kimi_profile", http.MethodGet, "/coding/v1/me", func(id, email, plan any) map[string]any {
		m := map[string]any{"region": "REGION_OVERSEA"}
		put(m, "user_id", id)
		put(m, "email", email)
		put(m, "user_level_name", plan)
		return m
	}},
	{"kimi global", ProviderKimi, KimiGlobalIntegration, "kimi_profile", http.MethodGet, "/coding/v1/me", func(id, email, plan any) map[string]any {
		m := map[string]any{}
		put(m, "user_id", id)
		put(m, "email", email)
		put(m, "user_level_name", plan)
		return m
	}},
}

const (
	profKey    = "synthetic-profile-key-1"
	profKeyAlt = "synthetic-profile-key-2"
)

// profEnv is a fake OpenCode credential service plus a fake provider profile endpoint.
type profEnv struct {
	t    *testing.T
	pc   profCase
	oc   *counted
	prov *counted
	key  atomic.Value
	c    Collector
}

func (e *profEnv) setKey(k string) { e.key.Store(k) }

func (e *profEnv) ident(key string) string {
	return keyedCredentialIdentity(identity(e.pc.integration+":c1"), key)
}

func (e *profEnv) profile(expected string) (model.ProfileUpdate, error) {
	e.t.Helper()
	pc, ok := e.c.(ProfileCollector)
	if !ok {
		e.t.Fatal("not a ProfileCollector")
	}
	return pc.FetchProfile(context.Background(), expected)
}

func newProfEnv(t *testing.T, pc profCase, tune func(*Spec), h func(e *profEnv, w http.ResponseWriter, r *http.Request)) *profEnv {
	t.Helper()
	e := &profEnv{t: t, pc: pc}
	e.setKey(profKey)
	e.oc = serve(t, func(w http.ResponseWriter, r *http.Request) {
		k, _ := e.key.Load().(string)
		entries := []map[string]any{}
		if k != "" {
			entries = append(entries, map[string]any{"id": "c1", "integrationID": pc.integration, "active": true, "value": map[string]any{"type": "key", "key": k}})
		}
		w.Write(mustJSON(map[string]any{"data": entries}))
	})
	e.prov = serve(t, func(w http.ResponseWriter, r *http.Request) { h(e, w, r) })
	spec := keyedSpec(pc.provider, pc.integration, e.oc.URL)
	if tune != nil {
		tune(&spec)
	}
	c, err := New(spec, opts(map[string]string{pc.endpointKey: e.prov.URL + pc.path}))
	if err != nil {
		t.Fatal(err)
	}
	e.c = c
	return e
}

// checkedHandler validates method, path and Authorization, then writes body.
func checkedHandler(body func() []byte) func(*profEnv, http.ResponseWriter, *http.Request) {
	return func(e *profEnv, w http.ResponseWriter, r *http.Request) {
		if r.Method != e.pc.method || r.URL.Path != e.pc.path {
			e.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+profKey {
			e.t.Error("wrong provider authorization")
		}
		if r.ContentLength > 0 {
			e.t.Error("profile request must have no body")
		}
		jsonHandler(body())(w, r)
	}
}

func staticBody(b []byte) func() []byte { return func() []byte { return b } }

func TestKeyedProfileFields(t *testing.T) {
	for _, pc := range profCases {
		for _, tc := range []struct {
			name                string
			id, email, plan     any
			wantEmail, wantPlan string
		}{
			{"known", "u1", "  person@example.test ", "  Max ", "person@example.test", "Max"},
			{"null", "u1", nil, nil, "", ""},
			{"missing", "u1", absent{}, absent{}, "", ""},
			{"invalid types", "u1", 123, []string{"max"}, "", ""},
			{"not email", "u1", "organization-123", "pro", "", "pro"},
			{"display name email", "u1", "Person <person@example.test>", "pro", "", "pro"},
			{"controls", "u1", "person@example.test\n", "Max\t", "", ""},
			{"format char", "u1", "per\u200bson@example.test", "Ma\u200bx", "", ""},
			{"long", "u1", strings.Repeat("a", 301) + "@example.test", strings.Repeat("p", 101), "", ""},
			{"max plan length", "u1", "person@example.test", strings.Repeat("p", 100), "person@example.test", strings.Repeat("p", 100)},
			{"empty strings", "u1", " ", "", "", ""},
		} {
			t.Run(pc.name+"/"+tc.name, func(t *testing.T) {
				e := newProfEnv(t, pc, nil, checkedHandler(staticBody(mustJSON(pc.build(tc.id, tc.email, tc.plan)))))
				want := e.ident(profKey)
				upd, err := e.profile(want)
				if err != nil || upd.Failure != nil || upd.Profile == nil || !upd.IdentityChecked || upd.CurrentIdentity != want {
					t.Fatalf("update %+v err %v", upd, err)
				}
				p := upd.Profile
				if p.Identity != want || p.Invalidated || profileValue(p.AccountEmail) != tc.wantEmail || profileValue(p.Plan) != tc.wantPlan {
					t.Fatal("unexpected profile")
				}
				if (tc.wantEmail == "") != (p.AccountEmail == nil) || (tc.wantPlan == "") != (p.Plan == nil) {
					t.Fatal("absent fields must be nil")
				}
				if e.prov.hits.Load() != 1 || e.oc.hits.Load() != 2 {
					t.Fatalf("requests provider=%d credentials=%d", e.prov.hits.Load(), e.oc.hits.Load())
				}
			})
		}
	}
}

func TestKeyedProfileRequiresAccountIdentity(t *testing.T) {
	for _, pc := range profCases {
		for name, body := range map[string]string{
			"no id":        string(mustJSON(pc.build(absent{}, "person@example.test", "Max"))),
			"null id":      string(mustJSON(pc.build(nil, "person@example.test", "Max"))),
			"empty id":     string(mustJSON(pc.build(" ", "person@example.test", "Max"))),
			"numeric id":   string(mustJSON(pc.build(42, "person@example.test", "Max"))),
			"usage json":   string(fixture(t, "ollama_legacy.json")),
			"array":        `[{"ID":"u1"}]`,
			"null":         `null`,
			"string":       `"u1"`,
			"malformed":    `{"ID":`,
			"empty object": `{}`,
			"empty body":   ``,
		} {
			t.Run(pc.name+"/"+name, func(t *testing.T) {
				e := newProfEnv(t, pc, nil, checkedHandler(staticBody([]byte(body))))
				upd, err := e.profile(e.ident(profKey))
				ei := errInfo(t, err)
				if ei.Code != CodeInvalidResponse || upd.Profile != nil || upd.Failure == nil || upd.Failure.Code != CodeInvalidResponse {
					t.Fatalf("%+v %+v", upd, ei)
				}
				if !upd.IdentityChecked || upd.CurrentIdentity != e.ident(profKey) {
					t.Fatal("identity must still be reported")
				}
			})
		}
	}
}

func TestKeyedProfileWrongExpectationMakesNoProviderCall(t *testing.T) {
	for _, pc := range profCases {
		for _, expected := range []string{"", "key:other", "other"} {
			t.Run(pc.name+"/"+expected, func(t *testing.T) {
				e := newProfEnv(t, pc, nil, checkedHandler(staticBody(mustJSON(pc.build("u1", "person@example.test", "Max")))))
				upd, err := e.profile(expected)
				if errInfo(t, err).Code != CodeProfileIdentityChanged || upd.Failure == nil || upd.Failure.Code != CodeProfileIdentityChanged {
					t.Fatalf("%+v", upd)
				}
				if e.prov.hits.Load() != 0 || upd.Profile != nil || !upd.IdentityChecked || upd.CurrentIdentity != e.ident(profKey) {
					t.Fatal("must report the new identity without calling the provider")
				}
				if upd.ExpectedIdentity != expected {
					t.Fatal("expected identity must be echoed")
				}
			})
		}
	}
}

func TestKeyedProfileKeyChangedAfterExpectation(t *testing.T) {
	for _, pc := range profCases {
		t.Run(pc.name, func(t *testing.T) {
			e := newProfEnv(t, pc, nil, checkedHandler(staticBody(mustJSON(pc.build("u1", "person@example.test", "Max")))))
			old := e.ident(profKey)
			e.setKey(profKeyAlt)
			upd, err := e.profile(old)
			if errInfo(t, err).Code != CodeProfileIdentityChanged || e.prov.hits.Load() != 0 || upd.CurrentIdentity != e.ident(profKeyAlt) {
				t.Fatalf("%+v", upd)
			}
		})
	}
}

func TestKeyedProfileCredentialSwitchMidResponse(t *testing.T) {
	for _, pc := range profCases {
		t.Run(pc.name, func(t *testing.T) {
			body := mustJSON(pc.build("u1", "person@example.test", "Max"))
			e := newProfEnv(t, pc, nil, func(e *profEnv, w http.ResponseWriter, r *http.Request) {
				e.setKey(profKeyAlt)
				jsonHandler(body)(w, r)
			})
			old := e.ident(profKey)
			upd, err := e.profile(old)
			if errInfo(t, err).Code != CodeProfileIdentityChanged || upd.Profile != nil {
				t.Fatalf("late profile applied: %+v", upd)
			}
			if !upd.IdentityChecked || upd.CurrentIdentity != e.ident(profKeyAlt) || upd.ExpectedIdentity != old {
				t.Fatal("new identity must be reported")
			}
		})
	}
}

func TestKeyedProfileCredentialGoneMidResponse(t *testing.T) {
	for _, pc := range profCases {
		t.Run(pc.name, func(t *testing.T) {
			body := mustJSON(pc.build("u1", "person@example.test", "Max"))
			e := newProfEnv(t, pc, nil, func(e *profEnv, w http.ResponseWriter, r *http.Request) {
				e.setKey("")
				jsonHandler(body)(w, r)
			})
			upd, err := e.profile(e.ident(profKey))
			if errInfo(t, err).Code != CodeCredentialsUnavailable || upd.Profile != nil || !upd.IdentityChecked || upd.CurrentIdentity != "" {
				t.Fatalf("%+v", upd)
			}
		})
	}
}

func TestKeyedProfileCredentialsUnavailableAtStart(t *testing.T) {
	for _, pc := range profCases {
		t.Run(pc.name, func(t *testing.T) {
			e := newProfEnv(t, pc, nil, checkedHandler(staticBody(mustJSON(pc.build("u1", "", "")))))
			e.setKey("")
			upd, err := e.profile("key:anything")
			if errInfo(t, err).Code != CodeCredentialsUnavailable || e.prov.hits.Load() != 0 || !upd.IdentityChecked || upd.CurrentIdentity != "" || upd.Profile != nil {
				t.Fatalf("%+v", upd)
			}
		})
	}
}

func TestKeyedProfileRateLimitKeepsRetryAfter(t *testing.T) {
	for _, pc := range profCases {
		for _, switched := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/switched=%v", pc.name, switched), func(t *testing.T) {
				e := newProfEnv(t, pc, nil, func(e *profEnv, w http.ResponseWriter, r *http.Request) {
					if switched {
						e.setKey(profKeyAlt)
					}
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(http.StatusTooManyRequests)
				})
				upd, err := e.profile(e.ident(profKey))
				ei := errInfo(t, err)
				want := fakeNow.Add(120 * time.Second)
				if ei.Code != CodeRateLimited || ei.HTTPStatus != 429 || ei.RetryAt == nil || !ei.RetryAt.Equal(want) || upd.Failure == nil || upd.Profile != nil {
					t.Fatalf("%+v %+v", upd, ei)
				}
				wantCur := e.ident(profKey)
				if switched {
					wantCur = e.ident(profKeyAlt)
				}
				if !upd.IdentityChecked || upd.CurrentIdentity != wantCur {
					t.Fatal("identity must reflect the post-response read")
				}
			})
		}
	}
}

func TestKeyedProfileStatusAndTransportErrors(t *testing.T) {
	for _, pc := range profCases {
		for status, code := range map[int]string{401: CodeAuthRequired, 403: CodeAccessDenied, 404: CodeInvalidResponse, 500: CodeUpstreamError} {
			t.Run(fmt.Sprintf("%s/%d", pc.name, status), func(t *testing.T) {
				e := newProfEnv(t, pc, nil, func(e *profEnv, w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
				upd, err := e.profile(e.ident(profKey))
				if ei := errInfo(t, err); ei.Code != code || ei.HTTPStatus != status || upd.Profile != nil || !upd.IdentityChecked || upd.CurrentIdentity != e.ident(profKey) {
					t.Fatalf("%+v", upd)
				}
			})
		}
		t.Run(pc.name+"/redirect", func(t *testing.T) {
			other := serve(t, func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") })
			e := newProfEnv(t, pc, nil, func(e *profEnv, w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL, http.StatusFound)
			})
			_, err := e.profile(e.ident(profKey))
			if errInfo(t, err).Code != CodeInvalidResponse || other.hits.Load() != 0 {
				t.Fatal("redirect")
			}
		})
		t.Run(pc.name+"/oversized", func(t *testing.T) {
			e := newProfEnv(t, pc, nil, func(e *profEnv, w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"ID":"u1","id":"`))
				w.Write([]byte(strings.Repeat("a", maxProfileBytes)))
				w.Write([]byte(`"}`))
			})
			upd, err := e.profile(e.ident(profKey))
			if errInfo(t, err).Code != CodeInvalidResponse || upd.Profile != nil {
				t.Fatal("oversized body accepted")
			}
		})
		t.Run(pc.name+"/timeout", func(t *testing.T) {
			e := newProfEnv(t, pc, func(s *Spec) { s.Timeout = 80 * time.Millisecond }, func(e *profEnv, w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			})
			upd, err := e.profile(e.ident(profKey))
			if errInfo(t, err).Code != CodeTimeout || upd.Profile != nil {
				t.Fatalf("%+v", upd)
			}
		})
		t.Run(pc.name+"/canceled", func(t *testing.T) {
			e := newProfEnv(t, pc, nil, checkedHandler(staticBody(mustJSON(pc.build("u1", "", "")))))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			upd, err := e.c.(ProfileCollector).FetchProfile(ctx, e.ident(profKey))
			if upd.Profile != nil || err == nil || e.prov.hits.Load() != 0 {
				t.Fatalf("%+v %v", upd, err)
			}
		})
	}
}

func TestKeyedProfileNeverExposesSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Chdir(dir)
	for _, pc := range profCases {
		t.Run(pc.name, func(t *testing.T) {
			raw := "raw-profile-body-marker"
			for _, body := range [][]byte{mustJSON(pc.build("u1", "person@example.test", raw)), []byte(`{"broken":"` + raw)} {
				e := newProfEnv(t, pc, nil, checkedHandler(staticBody(body)))
				upd, err := e.profile(e.ident(profKey))
				out := fmt.Sprintf("%v %+v %#v %+v", err, err, err, upd)
				if upd.Profile != nil {
					j, _ := json.Marshal(upd.Profile)
					out += string(j)
				}
				for _, secret := range []string{profKey, profKeyAlt, e.prov.URL, e.oc.URL, "Bearer"} {
					if strings.Contains(out, secret) {
						t.Fatal("secret or endpoint leaked")
					}
				}
				if upd.Profile == nil && strings.Contains(out, raw) {
					t.Fatal("raw body leaked into failure")
				}
			}
		})
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("collector wrote files")
	}
}

func TestKeyedFetchCarriesProfileIdentity(t *testing.T) {
	for _, pc := range profCases {
		usage := map[string]string{ProviderOllama: "ollama_legacy.json", ProviderKimi: "kimi_live.json"}[pc.provider]
		usageKey := map[string]string{ProviderOllama: "ollama", ProviderKimi: "kimi"}[pc.provider]
		build := func(t *testing.T, h http.HandlerFunc) (*profEnv, Collector) {
			e := newProfEnv(t, pc, nil, func(e *profEnv, w http.ResponseWriter, r *http.Request) {
				t.Error("profile endpoint requested by usage fetch")
			})
			prov := serve(t, h)
			c, err := New(keyedSpec(pc.provider, pc.integration, e.oc.URL), opts(map[string]string{usageKey: prov.URL}))
			if err != nil {
				t.Fatal(err)
			}
			return e, c
		}
		t.Run(pc.name+"/success", func(t *testing.T) {
			e, c := build(t, jsonHandler(fixture(t, usage)))
			res, err := c.Fetch(context.Background())
			if err != nil || res.ProfileIdentity != e.ident(profKey) || res.Identity != identity(pc.integration+":c1") || res.Profile != nil {
				t.Fatalf("identity %q err %v", res.ProfileIdentity, err)
			}
			if strings.Contains(res.ProfileIdentity, profKey) {
				t.Fatal("identity contains key")
			}
		})
		t.Run(pc.name+"/key generation", func(t *testing.T) {
			e, c := build(t, jsonHandler(fixture(t, usage)))
			a, _ := c.Fetch(context.Background())
			e.setKey(profKeyAlt)
			b, _ := c.Fetch(context.Background())
			if a.ProfileIdentity == b.ProfileIdentity || a.Identity != b.Identity {
				t.Fatal("rotating the key must change only the profile identity")
			}
		})
		t.Run(pc.name+"/usage errors", func(t *testing.T) {
			for name, h := range map[string]http.HandlerFunc{
				"500":       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
				"malformed": jsonHandler([]byte(`{`)),
				"no data":   jsonHandler([]byte(`{}`)),
			} {
				e, c := build(t, h)
				_, err := c.Fetch(context.Background())
				ei := errInfo(t, err)
				if !ei.ProfileIdentityChecked || ei.ProfileIdentity != e.ident(profKey) {
					t.Fatalf("%s: %+v", name, ei)
				}
				if strings.Contains(fmt.Sprintf("%+v", ei), profKey) {
					t.Fatal("key leaked")
				}
			}
		})
		t.Run(pc.name+"/transport error", func(t *testing.T) {
			e, _ := build(t, func(w http.ResponseWriter, r *http.Request) {})
			dead := serve(t, func(w http.ResponseWriter, r *http.Request) {})
			dead.Close()
			c, _ := New(keyedSpec(pc.provider, pc.integration, e.oc.URL), opts(map[string]string{usageKey: dead.URL}))
			_, err := c.Fetch(context.Background())
			if ei := errInfo(t, err); ei.Code != CodeNetworkError || !ei.ProfileIdentityChecked || ei.ProfileIdentity != e.ident(profKey) {
				t.Fatalf("%+v", ei)
			}
		})
		t.Run(pc.name+"/credential source failure", func(t *testing.T) {
			e, c := build(t, jsonHandler(fixture(t, usage)))
			e.setKey("")
			_, err := c.Fetch(context.Background())
			if ei := errInfo(t, err); ei.Code != CodeCredentialsUnavailable || !ei.ProfileIdentityChecked || ei.ProfileIdentity != "" {
				t.Fatalf("%+v", ei)
			}
		})
	}
}

func TestProfileEndpointDefaultsAndOverrides(t *testing.T) {
	oc := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, tc := range []struct {
		provider, integration, wantHost, wantPath string
	}{
		{ProviderOllama, "ollama-cloud", "ollama.com", "/api/me"},
		{ProviderKimi, "kimi-code-plan-cn", "api.kimi.com", "/coding/v1/me"},
		{ProviderKimi, KimiGlobalIntegration, "api.kimi.ai", "/coding/v1/me"},
	} {
		c, err := New(keyedSpec(tc.provider, tc.integration, oc.URL), opts(nil))
		if err != nil {
			t.Fatal(err)
		}
		var ep string
		switch v := c.(type) {
		case *ollamaCollector:
			ep = v.profileEndpoint
		case *kimiCollector:
			ep = v.profileEndpoint
		}
		u, err := url.Parse(ep)
		if err != nil || u.Scheme != "https" || u.Host != tc.wantHost || u.Path != tc.wantPath || u.RawQuery != "" {
			t.Fatalf("%s %s: %q", tc.provider, tc.integration, ep)
		}
	}
	for _, k := range []string{"ollama_profile", "kimi_profile"} {
		provider, integration := ProviderOllama, "ollama-cloud"
		if k == "kimi_profile" {
			provider, integration = ProviderKimi, "kimi-code-plan-cn"
		}
		for _, bad := range []string{"file:///etc/passwd", "ftp://example.test/me", "https://user:pw@example.test/me", "not a url", ""} {
			if _, err := New(keyedSpec(provider, integration, oc.URL), opts(map[string]string{k: bad})); err == nil || errInfo(t, err).Code != CodeConfigInvalid {
				t.Fatalf("%s accepted %q", k, bad)
			}
		}
	}
}
