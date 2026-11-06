package collectors

import (
	"context"
	"net/http"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPreserveReportedOverageAndRejectOverflow(t *testing.T) {
	res, err := parseCodex([]byte(`{"rate_limit":{"primary_window":{"used_percent":125,"limit_window_seconds":604800}}}`), fakeNow)
	if err != nil || *res.Quotas[0].UsedPercent != 125 || *res.Quotas[0].RemainingPercent != -25 {
		t.Fatalf("reported overage was silently clamped: %v", err)
	}
	if _, err := parseKimi([]byte(`{"usages":{"limit_5h":{"used_ratio":1e308}}}`), fakeNow); err == nil {
		t.Fatal("overflowing percentage accepted")
	}
}

func TestPlanLabelCannotLeakAnEmail(t *testing.T) {
	res, err := parseCodex([]byte(`{"plan_type":"person@example.com","rate_limit":{"primary_window":{"used_percent":25}}}`), fakeNow)
	if err != nil || res.Plan != nil {
		t.Fatal("unsafe plan label was published")
	}
}

func TestLongRetryAfterNotShortenedToOneHundredYears(t *testing.T) {
	const seconds = int64(200 * 365 * 24 * 3600)
	retry := parseRetryAfter("6307200000", fakeNow)
	if retry == nil || retry.Unix() != fakeNow.Unix()+seconds {
		t.Fatal("long server deadline was shortened or overflowed")
	}
}

func TestOpenCodeThrottlePreservesServerDeadline(t *testing.T) {
	oc := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, err := New(keyedSpec(ProviderOllama, "ollama-cloud", oc.URL), opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Fetch(context.Background())
	info := errInfo(t, err)
	if info.Code != CodeRateLimited || info.RetryAt == nil || !info.RetryAt.Equal(fakeNow.Add(24*time.Hour)) {
		t.Fatal("local credential API throttle was converted to an ordinary missing key")
	}
}

func TestCredentialFIFOIsRejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential-fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readBounded(path, maxFileBytes); ok {
		t.Fatal("non-regular credential source accepted")
	}
}

func TestLocalCredentialTransportNeverUsesProxy(t *testing.T) {
	c, err := New(keyedSpec(ProviderOllama, "ollama-cloud", "http://192.168.50.10:1234"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := c.(*ollamaCollector).oc.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("local credential response could transit an environment HTTP proxy")
	}
}
