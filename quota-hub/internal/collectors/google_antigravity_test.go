package collectors

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"quota-hub/internal/model"
)

// Synthetic fixtures only; tests never read a real workspace or credential.
const googleCredentials = `{"data":[
 {"id":"cred-inactive","integrationID":"google","active":false,"value":{"type":"oauth","methodID":"antigravity","access":"syn-inactive-token","refresh":"rt-unused","expires":1800000000000}},
 {"id":"cred-google-1","integrationID":"google","active":true,"value":{"type":"oauth","methodID":"antigravity","access":"syn-google-access-token","refresh":"rt-never-used","expires":1800000000000,"metadata":{"antigravityAccountId":"acct-synthetic-1","email":"person@example.invalid"}}}]}`

const googleQuotaSummary = `{
  "groups": [
    {
      "displayName": "Gemini Models",
      "buckets": [
        {
          "bucketId": "gemini-weekly",
          "displayName": "Weekly Limit Remaining",
          "window": "weekly",
          "resetTime": "2026-11-30T21:21:02Z",
          "remainingFraction": 0.86
        },
        {
          "bucketId": "gemini-5h",
          "displayName": "Five Hour Limit Remaining",
          "window": "5h",
          "resetTime": "2026-11-24T02:21:02Z",
          "remainingFraction": 0.485
        }
      ]
    },
    {
      "displayName": "Claude and GPT models",
      "buckets": [
        {
          "bucketId": "3p-weekly",
          "displayName": "Weekly Limit Remaining",
          "window": "weekly",
          "resetTime": "2026-12-01T00:06:53Z",
          "remainingFraction": 1
        },
        {
          "bucketId": "3p-5h",
          "displayName": "Five Hour Limit Remaining",
          "window": "5h",
          "resetTime": "2026-11-24T05:06:53Z",
          "remainingFraction": 0.9
        }
      ]
    }
  ]
}`

const googleLoadCodeAssist = `{
  "currentTier": {"id": "standard-tier", "name": "Antigravity"},
  "cloudaicompanionProject": "synthetic-project",
  "paidTier": {"id": "g1-pro-tier", "name": "Google AI Pro"}
}`

func googleSpec(serviceFile string) Spec {
	return Spec{
		Definition:          model.Definition{ID: "google-primary", Provider: ProviderGoogle, Source: SourceOpenCodeAntigravity, Product: "google_antigravity"},
		IntegrationID:       GoogleAntigravityIntegration,
		OpenCodeServiceFile: serviceFile,
	}
}

func newGoogleForTest(t *testing.T, credentials string, quotaHandler, profileHandler http.HandlerFunc) Collector {
	t.Helper()
	oc := serve(t, openCodeHandler(t, []byte(credentials), basicFor(testServicePassword)))
	prov := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:retrieveUserQuotaSummary":
			if quotaHandler != nil {
				quotaHandler(w, r)
				return
			}
		case "/v1internal:loadCodeAssist":
			if profileHandler != nil {
				profileHandler(w, r)
				return
			}
		}
		t.Errorf("unexpected Google path %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	spec := googleSpec(registrationFor(t, oc.URL, testServicePassword))
	c, err := New(spec, opts(map[string]string{
		"google_quota":   prov.URL + "/v1internal:retrieveUserQuotaSummary",
		"google_profile": prov.URL + "/v1internal:loadCodeAssist",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGoogleAntigravityFetchesSummaryAndOrdersGeminiFiveHourFirst(t *testing.T) {
	requests := 0
	c := newGoogleForTest(t, googleCredentials, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			t.Errorf("unexpected method/query: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer syn-google-access-token" {
			t.Errorf("wrong auth header: %q", r.Header.Get("Authorization"))
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "antigravity/hub/") {
			t.Errorf("expected Antigravity Hub User-Agent, got %q", r.Header.Get("User-Agent"))
		}
		body, _ := io.ReadAll(r.Body)
		if strings.TrimSpace(string(body)) != "{}" {
			t.Errorf("unexpected request body: %q", string(body))
		}
		w.Write([]byte(googleQuotaSummary))
	}, nil)

	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || res.Source != SourceOpenCodeAntigravity || len(res.Quotas) != 4 {
		t.Fatalf("unexpected result: requests=%d source=%q quotas=%d", requests, res.Source, len(res.Quotas))
	}
	wantIDs := []string{"gemini_5h", "gemini_weekly", "3p_5h", "3p_weekly"}
	wantScopes := []string{"Gemini", "Gemini", "Claude / GPT", "Claude / GPT"}
	wantModels := []string{"gemini", "gemini", "claude,gpt", "claude,gpt"}
	wantUsed := []float64{51.5, 14, 10, 0}
	for i := range wantIDs {
		q := res.Quotas[i]
		if q.ID != wantIDs[i] || q.Scope == nil || *q.Scope != wantScopes[i] ||
			q.Model == nil || *q.Model != wantModels[i] ||
			q.UsedPercent == nil || math.Abs(*q.UsedPercent-wantUsed[i]) > 1e-6 ||
			q.RemainingPercent == nil || math.Abs(*q.RemainingPercent-(100-wantUsed[i])) > 1e-6 ||
			q.Limited == nil || !*q.Limited || q.ObservedAt == nil || q.Window.ResetAt == nil {
			t.Fatalf("quota[%d] mismatch: %+v", i, q)
		}
	}
	if *res.Quotas[0].Window.DurationSeconds != 18000 || res.Quotas[0].Window.Period != nil {
		t.Fatalf("gemini_5h window: %+v", res.Quotas[0].Window)
	}
	if *res.Quotas[1].Window.DurationSeconds != 604800 || res.Quotas[1].Window.Period == nil || *res.Quotas[1].Window.Period != "week" {
		t.Fatalf("gemini_weekly window: %+v", res.Quotas[1].Window)
	}
	if res.Identity == "" || res.ProfileIdentity == "" {
		t.Fatal("missing private identity fields")
	}
	raw, _ := json.Marshal(res)
	for _, secret := range []string{"syn-google-access-token", "syn-inactive-token", "rt-never-used", "person@example.invalid", "acct-synthetic-1"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("credential data leaked: %s", secret)
		}
	}
}

func TestGoogleAntigravityIncludesArgonUnknownBucketsAndExhaustedZero(t *testing.T) {
	body := `{
	  "groups": [
	    {
	      "displayName": "Gemini 4 Argon",
	      "buckets": [
	        {"bucketId": "gemini-argon-wtu-weekly", "window": "weekly", "resetTime": "2026-11-30T10:00:00Z", "remainingFraction": 0.25}
	      ]
	    },
	    {
	      "displayName": "Gemini Models",
	      "buckets": [
	        {"bucketId": "gemini-5h", "window": "5h", "resetTime": "2026-11-24T02:00:00Z"},
	        {"bucketId": "gemini-disabled", "window": "5h", "resetTime": "2026-11-24T02:00:00Z", "disabled": true, "remainingFraction": 1},
	        {"bucketId": "gemini-unmeasured", "window": "5h"}
	      ]
	    },
	    {
	      "displayName": "Future Pool",
	      "buckets": [
	        {"bucketId": "future-custom", "displayName": "Custom Burst", "window": "burst", "resetTime": "2026-11-24T03:00:00Z", "remaining": {"remainingFraction": 0.6}}
	      ]
	    }
	  ]
	}`
	quotas, err := parseGoogleQuotaSummary([]byte(body), fakeNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(quotas) != 3 {
		t.Fatalf("want 3 quotas, got %d: %+v", len(quotas), quotas)
	}
	// gemini_5h omitted remainingFraction with resetTime => proto3 0.0 remaining => 100% used.
	if quotas[0].ID != "gemini_5h" || *quotas[0].UsedPercent != 100 || *quotas[0].RemainingPercent != 0 {
		t.Fatalf("exhausted gemini_5h: %+v", quotas[0])
	}
	if quotas[1].ID != "gemini_argon_wtu_weekly" || *quotas[1].Scope != "Gemini 4 Argon" ||
		quotas[1].Model == nil || *quotas[1].Model != "argon" || *quotas[1].UsedPercent != 75 {
		t.Fatalf("argon bucket: %+v", quotas[1])
	}
	if quotas[2].ID != "future_custom" || *quotas[2].Scope != "Future Pool" ||
		quotas[2].Label == nil || *quotas[2].Label != "Custom Burst" || quotas[2].Model != nil ||
		math.Abs(*quotas[2].UsedPercent-40) > 1e-6 {
		t.Fatalf("future bucket: %+v", quotas[2])
	}
}

func TestGoogleAntigravityFetchProfileReadsTierAndRechecksCredential(t *testing.T) {
	c := newGoogleForTest(t, googleCredentials, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(googleQuotaSummary))
	}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"ANTIGRAVITY"`) {
			t.Errorf("unexpected loadCodeAssist payload: %s", string(body))
		}
		w.Write([]byte(googleLoadCodeAssist))
	})
	res, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pc, ok := c.(ProfileCollector)
	if !ok {
		t.Fatal("google collector must implement ProfileCollector")
	}
	upd, err := pc.FetchProfile(context.Background(), res.ProfileIdentity)
	if err != nil || upd.Profile == nil || !upd.IdentityChecked || upd.CurrentIdentity != res.ProfileIdentity {
		t.Fatalf("FetchProfile: %+v %v", upd, err)
	}
	if upd.Profile.Plan == nil || *upd.Profile.Plan != "Google AI Pro" ||
		upd.Profile.AccountEmail == nil || *upd.Profile.AccountEmail != "person@example.invalid" {
		t.Fatalf("wrong profile: %+v", upd.Profile)
	}

	// Mismatched expected identity rejects without hitting upstream.
	if _, err := pc.FetchProfile(context.Background(), "stale-identity"); err == nil || errInfo(t, err).Code != CodeProfileIdentityChanged {
		t.Fatalf("expected profile_identity_changed, got %v", err)
	}

	// Concurrent account switch while loadCodeAssist is in flight suppresses profile.
	switched := false
	oc := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if switched {
			w.Write([]byte(strings.Replace(googleCredentials, `"acct-synthetic-1"`, `"acct-synthetic-2"`, 1)))
			return
		}
		w.Write([]byte(googleCredentials))
	}))
	prov := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			switched = true
			w.Write([]byte(googleLoadCodeAssist))
			return
		}
		w.Write([]byte(googleQuotaSummary))
	}))
	c2, err := New(googleSpec(registrationFor(t, oc.URL, "")), opts(map[string]string{
		"google_quota":   prov.URL + "/v1internal:retrieveUserQuotaSummary",
		"google_profile": prov.URL + "/v1internal:loadCodeAssist",
	}))
	if err != nil {
		t.Fatal(err)
	}
	res2, err := c2.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if upd2, err := c2.(ProfileCollector).FetchProfile(context.Background(), res2.ProfileIdentity); err == nil || upd2.Profile != nil || errInfo(t, err).Code != CodeProfileIdentityChanged {
		t.Fatalf("concurrent switch must reject profile, got upd=%+v err=%v", upd2, err)
	}
}

func TestGoogleAntigravityProfileTierFallbacksAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		wantErr          bool
	}{
		{"paid name", `{"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"},"currentTier":{"name":"Antigravity"}}`, "Google AI Pro", false},
		{"paid id fallback", `{"paidTier":{"id":"g1-ultra-tier"}}`, "Google AI Ultra", false},
		{"current tier fallback", `{"currentTier":{"name":"Antigravity"}}`, "Antigravity", false},
		{"project only", `{"cloudaicompanionProject":"p"}`, "", false},
		{"unrelated object", `{"ok":true}`, "", true},
		{"not object", `[]`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := parseGoogleProfile([]byte(tc.body))
			if tc.wantErr {
				if err == nil || errInfo(t, err).Code != CodeInvalidResponse {
					t.Fatalf("expected invalid_response, got plan=%v err=%v", plan, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if plan != nil {
				got = *plan
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestGoogleAntigravityRejectsExpiredWrongMethodAmbiguousAndMissingCredentials(t *testing.T) {
	for name, tc := range map[string]struct {
		creds string
		want  string
	}{
		"expired":       {strings.Replace(googleCredentials, `1800000000000`, `1700000000000`, 2), CodeAuthRequired},
		"wrong method":  {strings.Replace(googleCredentials, `"antigravity"`, `"gemini"`, 2), CodeCredentialsUnavailable},
		"api key":       {`{"data":[{"id":"k1","integrationID":"google","active":true,"value":{"type":"key","key":"AIza-fake"}}]}`, CodeCredentialsUnavailable},
		"ambiguous":     {strings.Replace(googleCredentials, `"active":false`, `"active":true`, 1), CodeCredentialsUnavailable},
		"missing":       {`{"data":[]}`, CodeCredentialsUnavailable},
		"bad expires":   {strings.Replace(googleCredentials, `1800000000000`, `-5`, 2), CodeCredentialsUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			c := newGoogleForTest(t, tc.creds, func(w http.ResponseWriter, r *http.Request) {
				t.Error("unexpected upstream request")
			}, nil)
			res, err := c.Fetch(context.Background())
			if err == nil || len(res.Quotas) != 0 {
				t.Fatalf("invalid credential succeeded: %+v", res)
			}
			if errInfo(t, err).Code != tc.want {
				t.Fatalf("got %v want %s", err, tc.want)
			}
		})
	}
}

func TestGoogleAntigravityRejectsInvalidPayloadsAndPropagatesErrors(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"groups":[]}`, `{"groups":[{"displayName":"Gemini","buckets":[]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":null,"resetTime":"2026-11-24T02:00:00Z"}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":-0.1,"resetTime":"2026-11-24T02:00:00Z"}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":1.1,"resetTime":"2026-11-24T02:00:00Z"}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5,"resetTime":"not-a-date"}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5,"disabled":"yes"}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remaining":"bad","resetTime":"2026-11-24T02:00:00Z"}]}]}`,
		`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5},{"bucketId":"gemini-5h","remainingFraction":0.6}]}]}`,
	} {
		c := newGoogleForTest(t, googleCredentials, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}, nil)
		if _, err := c.Fetch(context.Background()); err == nil || errInfo(t, err).Code != CodeInvalidResponse {
			t.Fatalf("accepted invalid body %s: %v", body, err)
		}
	}

	for _, status := range []int{401, 403, 429, 500} {
		c := newGoogleForTest(t, googleCredentials, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(status)
		}, nil)
		_, err := c.Fetch(context.Background())
		if err == nil || errInfo(t, err).HTTPStatus != status {
			t.Fatalf("status %d: %v", status, err)
		}
		if status == 429 && errInfo(t, err).RetryAt == nil {
			t.Fatal("429 lost Retry-After")
		}
	}
}
