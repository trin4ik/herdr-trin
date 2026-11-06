package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

var t0 = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type fake struct {
	calls atomic.Int32
	fn    func(ctx context.Context, n int) (model.Result, error)
}

func (f *fake) Fetch(ctx context.Context) (model.Result, error) {
	n := int(f.calls.Add(1))
	return f.fn(ctx, n)
}

func okResult(at time.Time) model.Result {
	return model.Result{ObservedAt: at, Raw: json.RawMessage(`{"secret":"RAWSECRET"}`),
		Quotas: []model.Quota{{ID: "w", Metric: "percent", Unit: "percent", Window: model.Window{Kind: "rolling"}}}}
}

func defsFor(ids ...string) []model.Definition {
	var d []model.Definition
	for _, id := range ids {
		d = append(d, model.Definition{ID: id, Provider: id, Product: "p", Source: "s", StaleAfter: time.Hour})
	}
	return d
}

func spec(id, scope string, interval time.Duration) collectors.Spec {
	return collectors.Spec{Definition: model.Definition{ID: id, Provider: id}, Interval: interval, Timeout: time.Second, Scope: scope}
}

func opts(c *clock) Options {
	return Options{Now: c.Now, BackoffBase: time.Minute, BackoffMax: 10 * time.Minute, AuthRetry: 2 * time.Hour}
}

func setup(t *testing.T, dir string, c *clock, f *fake, ids ...string) (*Engine, *store.Store) {
	t.Helper()
	s, err := store.Open(dir, defsFor(ids...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var ws []Worker
	for _, id := range ids {
		ws = append(ws, Worker{Spec: spec(id, "", 10*time.Minute), Collector: f})
	}
	e, err := New(s, ws, opts(c))
	if err != nil {
		t.Fatal(err)
	}
	return e, s
}

func TestSuccessSchedulesInterval(t *testing.T) {
	c := &clock{t: t0}
	f := &fake{fn: func(_ context.Context, _ int) (model.Result, error) { return okResult(c.Now()), nil }}
	e, s := setup(t, t.TempDir(), c, f, "a")
	ok, err := e.Poll(context.Background(), "a")
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	sub, _ := s.Get("a")
	if !sub.NextAttemptAt.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("next %v", sub.NextAttemptAt)
	}
	// not due: no collector call
	ok, _ = e.Poll(context.Background(), "a")
	if ok || f.calls.Load() != 1 {
		t.Fatal("polled before next_attempt_at")
	}
	c.Add(10 * time.Minute)
	if ok, _ = e.Poll(context.Background(), "a"); !ok || f.calls.Load() != 2 {
		t.Fatal("not polled when due")
	}
	if _, err := e.Poll(context.Background(), "zzz"); !errors.Is(err, ErrUnknownWorker) {
		t.Fatal(err)
	}
}

func TestFailureClassesAndSchedule(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
		wait time.Duration
	}{
		{"5xx", &model.ErrorInfo{Code: "upstream_error", HTTPStatus: 503}, "upstream_error", time.Minute},
		{"401", &model.ErrorInfo{Code: "unauthorized", HTTPStatus: 401}, "unauthorized", 2 * time.Hour},
		{"403", fmt.Errorf("wrapped: %w", &model.ErrorInfo{Code: "forbidden", HTTPStatus: 403}), "forbidden", 2 * time.Hour},
		{"429 no date", &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429}, "rate_limited", 10 * time.Minute},
		{"timeout", context.DeadlineExceeded, "timeout", time.Minute},
		{"unknown", errors.New("Get https://x?key=SECRET-TOKEN: boom"), "collection_error", time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: t0}
			f := &fake{fn: func(context.Context, int) (model.Result, error) { return model.Result{}, tc.err }}
			e, s := setup(t, t.TempDir(), c, f, "a")
			if ok, err := e.Poll(context.Background(), "a"); !ok || err != nil {
				t.Fatal(ok, err)
			}
			sub, _ := s.Get("a")
			if sub.Error == nil || sub.Error.Code != tc.code {
				t.Fatalf("%+v", sub.Error)
			}
			if got := sub.NextAttemptAt.Sub(t0); got != tc.wait {
				t.Fatalf("wait %v want %v", got, tc.wait)
			}
			b, _ := json.Marshal(s.Snapshot(t0))
			if strings.Contains(string(b), "SECRET-TOKEN") {
				t.Fatal("arbitrary error text leaked")
			}
		})
	}
}

func TestExponentialBackoffCapsAndResets(t *testing.T) {
	c := &clock{t: t0}
	fail := true
	f := &fake{fn: func(context.Context, int) (model.Result, error) {
		if fail {
			return model.Result{}, &model.ErrorInfo{Code: "upstream_error", HTTPStatus: 502}
		}
		return okResult(c.Now()), nil
	}}
	e, s := setup(t, t.TempDir(), c, f, "a")
	var waits []time.Duration
	for i := 0; i < 6; i++ {
		e.Poll(context.Background(), "a")
		sub, _ := s.Get("a")
		waits = append(waits, sub.NextAttemptAt.Sub(c.Now()))
		c.t = *sub.NextAttemptAt
	}
	want := []time.Duration{1, 2, 4, 8, 10, 10}
	for i := range want {
		if waits[i] != want[i]*time.Minute {
			t.Fatalf("waits %v", waits)
		}
	}
	fail = false
	e.Poll(context.Background(), "a")
	if s.Failures("a") != 0 {
		t.Fatal("failures not reset")
	}
}

func TestRateLimitNeverShortensServerDate(t *testing.T) {
	c := &clock{t: t0}
	retry := t0.Add(6 * time.Hour) // far beyond BackoffMax
	f := &fake{fn: func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retry}
	}}
	e, s := setup(t, t.TempDir(), c, f, "a")
	e.Poll(context.Background(), "a")
	sub, _ := s.Get("a")
	if sub.NextAttemptAt.Before(retry) {
		t.Fatalf("shortened: %v", sub.NextAttemptAt)
	}
	c.Add(time.Hour)
	if ok, _ := e.Poll(context.Background(), "a"); ok {
		t.Fatal("polled during cooldown")
	}
}

func TestRestartHonorsStoredCooldown(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	f := &fake{fn: func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "unauthorized", HTTPStatus: 401}
	}}
	e, s := setup(t, dir, c, f, "a")
	e.Poll(context.Background(), "a")
	s.Close()

	f2 := &fake{fn: func(context.Context, int) (model.Result, error) { return okResult(c.Now()), nil }}
	c.Add(time.Minute)
	e2, _ := setup(t, dir, c, f2, "a")
	if ok, _ := e2.Poll(context.Background(), "a"); ok || f2.calls.Load() != 0 {
		t.Fatal("restart ignored cooldown")
	}
	c.Add(2 * time.Hour)
	if ok, _ := e2.Poll(context.Background(), "a"); !ok {
		t.Fatal("not polled after cooldown")
	}
}

func TestSharedScopeSerializesAndRateLimitIsScopeWide(t *testing.T) {
	c := &clock{t: t0}
	var cur, max atomic.Int32
	var limited atomic.Bool
	fn := func(ctx context.Context, _ int) (model.Result, error) {
		n := cur.Add(1)
		for {
			m := max.Load()
			if n <= m || max.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
		if limited.Load() {
			return model.Result{}, &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429}
		}
		return okResult(c.Now()), nil
	}
	f := &fake{fn: fn}
	s, _ := store.Open(t.TempDir(), defsFor("a", "b"))
	defer s.Close()
	e, err := New(s, []Worker{
		{Spec: spec("a", "shared", time.Minute), Collector: f},
		{Spec: spec("b", "shared", time.Minute), Collector: f},
	}, opts(c))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func() { defer wg.Done(); e.Poll(context.Background(), id) }()
	}
	wg.Wait()
	if max.Load() != 1 || f.calls.Load() != 2 {
		t.Fatalf("max concurrent %d calls %d", max.Load(), f.calls.Load())
	}
	c.Add(time.Minute)
	limited.Store(true)
	e.Poll(context.Background(), "a")
	c.Add(30 * time.Second) // b is due by its own schedule but the scope is cooling down
	calls := f.calls.Load()
	if ok, _ := e.Poll(context.Background(), "b"); ok || f.calls.Load() != calls {
		t.Fatal("sibling polled during scope 429 cooldown")
	}
}

func TestSingleflight(t *testing.T) {
	c := &clock{t: t0}
	started, release := make(chan struct{}), make(chan struct{})
	f := &fake{fn: func(context.Context, int) (model.Result, error) {
		close(started)
		<-release
		return okResult(c.Now()), nil
	}}
	e, _ := setup(t, t.TempDir(), c, f, "a")
	done := make(chan bool)
	go func() { ok, _ := e.Poll(context.Background(), "a"); done <- ok }()
	<-started
	if ok, _ := e.Poll(context.Background(), "a"); ok {
		t.Fatal("second concurrent poll ran")
	}
	close(release)
	if !<-done || f.calls.Load() != 1 {
		t.Fatal("first poll lost")
	}
}

func TestCancellationRecordsNothing(t *testing.T) {
	c := &clock{t: t0}
	ctx, cancel := context.WithCancel(context.Background())
	f := &fake{fn: func(ctx context.Context, _ int) (model.Result, error) {
		cancel()
		<-ctx.Done()
		return model.Result{}, &model.ErrorInfo{Code: "unauthorized", HTTPStatus: 401}
	}}
	e, s := setup(t, t.TempDir(), c, f, "a")
	ok, err := e.Poll(ctx, "a")
	if ok || err == nil {
		t.Fatal(ok, err)
	}
	sub, _ := s.Get("a")
	if sub.Error != nil || sub.LastAttemptAt != nil {
		t.Fatalf("cancellation recorded: %+v", sub)
	}
}

func TestTimeoutIsRecordedAsTimeout(t *testing.T) {
	c := &clock{t: t0}
	f := &fake{fn: func(ctx context.Context, _ int) (model.Result, error) {
		<-ctx.Done()
		return model.Result{}, ctx.Err()
	}}
	s, _ := store.Open(t.TempDir(), defsFor("a"))
	defer s.Close()
	sp := spec("a", "", time.Minute)
	sp.Timeout = 20 * time.Millisecond
	e, _ := New(s, []Worker{{Spec: sp, Collector: f}}, opts(c))
	if ok, err := e.Poll(context.Background(), "a"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if sub, _ := s.Get("a"); sub.Error == nil || sub.Error.Code != "timeout" {
		t.Fatalf("%+v", sub.Error)
	}
}

func TestPanicBecomesCollectionError(t *testing.T) {
	c := &clock{t: t0}
	f := &fake{fn: func(context.Context, int) (model.Result, error) { panic("token=SECRET") }}
	e, s := setup(t, t.TempDir(), c, f, "a")
	e.Poll(context.Background(), "a")
	if sub, _ := s.Get("a"); sub.Error == nil || sub.Error.Code != "collection_error" {
		t.Fatalf("%+v", sub.Error)
	}
}

func TestOldObservedTimestampNotFreshened(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	obs := t0.Add(-20 * time.Minute)
	f := &fake{fn: func(context.Context, int) (model.Result, error) { return okResult(obs), nil }}
	e, s := setup(t, dir, c, f, "a")
	for i := 0; i < 3; i++ {
		e.Poll(context.Background(), "a")
		c.Add(10 * time.Minute)
	}
	sub, _ := s.Get("a")
	if !sub.LastSuccessAt.Equal(obs) {
		t.Fatalf("freshened: %v", sub.LastSuccessAt)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "history", "a.jsonl"))
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Fatalf("history lines %d", n)
	}
}

func TestRunPollsAndStopsWithoutLeaks(t *testing.T) {
	before := runtime.NumGoroutine()
	f := &fake{fn: func(_ context.Context, n int) (model.Result, error) { return okResult(time.Now()), nil }}
	s, _ := store.Open(t.TempDir(), defsFor("a", "b"))
	defer s.Close()
	e, err := New(s, []Worker{
		{Spec: spec("a", "", 20*time.Millisecond), Collector: f},
		{Spec: spec("b", "", 20*time.Millisecond), Collector: f},
	}, Options{Jitter: 0.2, BackoffBase: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- e.Run(ctx) }()
	for dl := time.Now().Add(5 * time.Second); f.calls.Load() < 4 && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if err := e.Run(ctx); !errors.Is(err, ErrRunning) {
		t.Fatal("second Run allowed:", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if f.calls.Load() < 4 {
		t.Fatalf("calls %d", f.calls.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}

func TestRunRespectsRestoredNextAttempt(t *testing.T) {
	dir := t.TempDir()
	s, _ := store.Open(dir, defsFor("a"))
	now := time.Now()
	s.Commit(model.Update{ID: "a", AttemptAt: now, NextAttemptAt: now.Add(time.Hour),
		Failure: &model.ErrorInfo{Code: "upstream_error", HTTPStatus: 500}})
	defer s.Close()
	f := &fake{fn: func(context.Context, int) (model.Result, error) { return okResult(time.Now()), nil }}
	e, _ := New(s, []Worker{{Spec: spec("a", "", 10*time.Millisecond), Collector: f}}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	e.Run(ctx)
	if f.calls.Load() != 0 {
		t.Fatal("Run ignored restored next_attempt_at")
	}
}

func TestNewValidation(t *testing.T) {
	s, _ := store.Open(t.TempDir(), defsFor("a"))
	defer s.Close()
	f := &fake{}
	bad := [][]Worker{
		{{Spec: spec("zzz", "", time.Minute), Collector: f}},
		{{Spec: spec("a", "", 0), Collector: f}},
		{{Spec: spec("a", "", time.Minute)}},
		{{Spec: spec("a", "", time.Minute), Collector: f}, {Spec: spec("a", "", time.Minute), Collector: f}},
	}
	for i, ws := range bad {
		if _, err := New(s, ws, Options{}); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestPerProviderPolicyFromSpec(t *testing.T) {
	c := &clock{t: t0}
	retryEarly, retryLate := t0.Add(time.Minute), t0.Add(3*time.Hour)
	var info atomic.Pointer[model.ErrorInfo]
	f := &fake{fn: func(context.Context, int) (model.Result, error) { return model.Result{}, info.Load() }}
	s, _ := store.Open(t.TempDir(), defsFor("a", "b"))
	defer s.Close()
	sa := spec("a", "", time.Minute)
	sa.BackoffBase, sa.BackoffMax, sa.RateLimitPause, sa.AuthRetry = 5*time.Second, 20*time.Second, 15*time.Minute, 30*time.Minute
	sb := spec("b", "", time.Minute) // falls back to Options
	e, err := New(s, []Worker{{Spec: sa, Collector: f}, {Spec: sb, Collector: f}}, opts(c))
	if err != nil {
		t.Fatal(err)
	}
	next := func(id string) time.Duration {
		sub, _ := s.Get(id)
		return sub.NextAttemptAt.Sub(t0)
	}
	info.Store(&model.ErrorInfo{Code: "upstream_error", HTTPStatus: 500})
	e.Poll(context.Background(), "a")
	e.Poll(context.Background(), "b")
	if next("a") != 5*time.Second || next("b") != time.Minute {
		t.Fatalf("a=%v b=%v", next("a"), next("b"))
	}
	info.Store(&model.ErrorInfo{Code: "unauthorized", HTTPStatus: 401})
	c.Add(time.Hour)
	e.Poll(context.Background(), "a")
	if got := s.Failures("a"); got != 2 {
		t.Fatal(got)
	}
	if sub, _ := s.Get("a"); sub.NextAttemptAt.Sub(c.Now()) != 30*time.Minute {
		t.Fatalf("auth wait %v", sub.NextAttemptAt.Sub(c.Now()))
	}
	// 429: pause floor beats a short server date
	info.Store(&model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retryEarly})
	c.t = t0
	c.Add(2 * time.Hour)
	base := c.Now()
	e.Poll(context.Background(), "a")
	sub, _ := s.Get("a")
	if sub.NextAttemptAt.Sub(base) != 15*time.Minute {
		t.Fatalf("pause %v", sub.NextAttemptAt.Sub(base))
	}
	if sub.Error.RetryAt == nil || !sub.Error.RetryAt.Equal(*sub.NextAttemptAt) {
		t.Fatalf("absolute retry_at not persisted: %+v", sub.Error)
	}
	// a later server date beats the pause, even beyond BackoffMax
	c.Add(time.Hour)
	info.Store(&model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retryLate})
	c.t = t0.Add(2 * time.Hour)
	c.t = sub.NextAttemptAt.Add(0)
	e.Poll(context.Background(), "a")
	sub, _ = s.Get("a")
	if sub.NextAttemptAt.Before(retryLate) {
		t.Fatalf("server date shortened: %v", sub.NextAttemptAt)
	}
}

func TestPositiveJitterNeverShortens429(t *testing.T) {
	c := &clock{t: t0}
	retry := t0.Add(20 * time.Minute)
	f := &fake{fn: func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retry}
	}}
	for i := 0; i < 30; i++ {
		s, _ := store.Open(t.TempDir(), defsFor("a"))
		sp := spec("a", "", time.Minute)
		sp.RateLimitPause = 15 * time.Minute
		o := opts(c)
		o.Jitter = 0.5
		e, _ := New(s, []Worker{{Spec: sp, Collector: f}}, o)
		e.Poll(context.Background(), "a")
		sub, _ := s.Get("a")
		if sub.NextAttemptAt.Before(retry) || sub.NextAttemptAt.Sub(t0) < 15*time.Minute {
			t.Fatalf("shortened: %v", sub.NextAttemptAt)
		}
		s.Close()
	}
}

func TestRestoredScope429OnlyDelaysSameScope(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s, _ := store.Open(dir, defsFor("a", "b", "x"))
	retry := t0.Add(2 * time.Hour)
	s.Commit(model.Update{ID: "a", AttemptAt: t0, NextAttemptAt: t0.Add(time.Minute),
		Failure: &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retry}})
	s.Close()
	s, _ = store.Open(dir, defsFor("a", "b", "x"))
	defer s.Close()
	f := &fake{fn: func(context.Context, int) (model.Result, error) { return okResult(c.Now()), nil }}
	e, _ := New(s, []Worker{
		{Spec: spec("a", "shared", time.Minute), Collector: f},
		{Spec: spec("b", "shared", time.Minute), Collector: f},
		{Spec: spec("x", "other", time.Minute), Collector: f},
	}, opts(c))
	c.Add(10 * time.Minute)
	if ok, _ := e.Poll(context.Background(), "b"); ok {
		t.Fatal("same-scope sibling polled during restored 429 cooldown")
	}
	if ok, _ := e.Poll(context.Background(), "x"); !ok {
		t.Fatal("unrelated scope delayed")
	}
	c.Add(2 * time.Hour)
	if ok, _ := e.Poll(context.Background(), "b"); !ok {
		t.Fatal("sibling not polled after cooldown")
	}
}
