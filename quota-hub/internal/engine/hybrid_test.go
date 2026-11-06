package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

type hfake struct {
	fetches, observes atomic.Int32
	fetchFn           func(ctx context.Context, n int) (model.Result, error)
	obsFn             func(ctx context.Context, n int) (model.Result, error)
}

func (h *hfake) Fetch(ctx context.Context) (model.Result, error) {
	return h.fetchFn(ctx, int(h.fetches.Add(1)))
}

func (h *hfake) Observe(ctx context.Context) (model.Result, error) {
	return h.obsFn(ctx, int(h.observes.Add(1)))
}

var (
	errNoBridge                            = &model.ErrorInfo{Code: "source_unavailable"}
	_           collectors.HybridCollector = (*hfake)(nil)
)

func passive(at time.Time) model.Result {
	r := okResult(at)
	r.Source = "opencode_observed"
	return r
}

func apiResult(at time.Time) model.Result {
	r := okResult(at)
	r.Source = "claude_oauth_usage"
	return r
}

func hdefs(ids ...string) []model.Definition {
	var d []model.Definition
	for _, id := range ids {
		src := "s"
		if id == "claude" {
			src = collectors.SourceOpenCodeObserved
		}
		d = append(d, model.Definition{ID: id, Provider: id, Product: "p", Source: src, StaleAfter: 30 * time.Minute})
	}
	return d
}

func hspec(id, scope string) collectors.Spec {
	sp := spec(id, scope, 5*time.Minute)
	sp.Definition.Source = collectors.SourceOpenCodeObserved
	sp.ClaudeIdleAfter = 10 * time.Minute
	sp.ObserveInterval = 30 * time.Second
	sp.AuthRetry = 30 * time.Minute
	return sp
}

func hsetup(t *testing.T, dir string, c *clock, h *hfake) (*Engine, *store.Store) {
	t.Helper()
	s, err := store.Open(dir, hdefs("claude"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e, err := New(s, []Worker{{Spec: hspec("claude", ""), Collector: h}}, opts(c))
	if err != nil {
		t.Fatal(err)
	}
	return e, s
}

func noObs(context.Context, int) (model.Result, error) { return model.Result{}, errNoBridge }

func apiOK(c *clock) func(context.Context, int) (model.Result, error) {
	return func(context.Context, int) (model.Result, error) { return apiResult(c.Now()), nil }
}

// step advances the fake clock and runs one scan if due.
func step(t *testing.T, e *Engine, c *clock, d time.Duration) {
	t.Helper()
	c.Add(d)
	if _, err := e.Poll(context.Background(), "claude"); err != nil {
		t.Fatal(err)
	}
}

func journal(t *testing.T, dir, kind string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, kind, "claude.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestHybridBusyBridgeNeverCallsAPI(t *testing.T) {
	c := &clock{t: t0}
	h := &hfake{fetchFn: apiOK(c)}
	h.obsFn = func(context.Context, int) (model.Result, error) { return passive(c.Now().Add(-5 * time.Second)), nil }
	e, s := hsetup(t, t.TempDir(), c, h)
	for i := 0; i < 50; i++ { // 25 minutes of continuous traffic
		step(t, e, c, 30*time.Second)
	}
	if h.fetches.Load() != 0 {
		t.Fatalf("remote GET while bridge is active: %d", h.fetches.Load())
	}
	sub, _ := s.GetAt("claude", c.Now())
	if sub.LastPassiveAt == nil || sub.Source != "opencode_observed" || sub.Stale {
		t.Fatalf("%+v", sub)
	}
	if sub.NextPollAt == nil || sub.NextPollAt.Before(sub.LastPassiveAt.Add(10*time.Minute)) {
		t.Fatalf("next poll %v passive %v", sub.NextPollAt, sub.LastPassiveAt)
	}
}

func TestHybridIdleFetchesImmediatelyThenEveryFiveMinutesAndAPIDoesNotMakeBridgeActive(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	h := &hfake{fetchFn: apiOK(c), obsFn: noObs}
	e, s := hsetup(t, dir, c, h)
	if ok, err := e.Poll(context.Background(), "claude"); !ok || err != nil || h.fetches.Load() != 1 {
		t.Fatalf("first scan must fetch at once: %v %v %d", ok, err, h.fetches.Load())
	}
	for i := 0; i < 40; i++ { // 20 minutes of 30s scans
		step(t, e, c, 30*time.Second)
	}
	if got := h.fetches.Load(); got != 5 { // 0, 5, 10, 15, 20 min
		t.Fatalf("fetches %d", got)
	}
	sub, _ := s.GetAt("claude", c.Now())
	if sub.LastPassiveAt != nil {
		t.Fatalf("API success moved the passive clock: %v", sub.LastPassiveAt)
	}
	if sub.Source != "claude_oauth_usage" || sub.Stale || sub.Error != nil {
		t.Fatalf("%+v", sub)
	}
	if n := journal(t, dir, "attempts"); n != 5 {
		t.Fatalf("local no-change scans logged attempts: %d", n)
	}
	if h.observes.Load() != 41 {
		t.Fatalf("observe must run on every scan: %d", h.observes.Load())
	}
}

func TestHybridResumingHeadersSuppressFetch(t *testing.T) {
	c := &clock{t: t0}
	var live atomic.Bool
	h := &hfake{fetchFn: apiOK(c)}
	h.obsFn = func(context.Context, int) (model.Result, error) {
		if live.Load() {
			return passive(c.Now()), nil
		}
		return model.Result{}, errNoBridge
	}
	e, s := hsetup(t, t.TempDir(), c, h)
	e.Poll(context.Background(), "claude") // fetch #1 at t0
	step(t, e, c, 5*time.Minute)           // fetch #2 at 5m
	if h.fetches.Load() != 2 {
		t.Fatal(h.fetches.Load())
	}
	c.Add(2 * time.Minute) // 7m: headers resume
	live.Store(true)
	e.Poll(context.Background(), "claude")
	sub, _ := s.GetAt("claude", c.Now())
	if sub.NextPollAt == nil || !sub.NextPollAt.Equal(t0.Add(17*time.Minute)) {
		t.Fatalf("next poll %v", sub.NextPollAt)
	}
	live.Store(false)
	for c.Now().Before(t0.Add(16*time.Minute + 30*time.Second)) {
		step(t, e, c, 30*time.Second)
	}
	if h.fetches.Load() != 2 {
		t.Fatalf("extra GET during the recent-bridge window: %d", h.fetches.Load())
	}
	step(t, e, c, 30*time.Second)
	if h.fetches.Load() != 3 {
		t.Fatalf("no fallback once idle again: %d", h.fetches.Load())
	}
}

func TestHybrid429PausePersistsWhileHeadersContinueAndAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	retry := t0.Add(time.Hour)
	var live atomic.Bool
	h := &hfake{}
	h.fetchFn = func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retry}
	}
	h.obsFn = func(context.Context, int) (model.Result, error) {
		if live.Load() {
			return passive(c.Now()), nil
		}
		return model.Result{}, errNoBridge
	}
	e, s := hsetup(t, dir, c, h)
	e.Poll(context.Background(), "claude")
	sub, _ := s.GetAt("claude", c.Now())
	if sub.Error == nil || sub.LastPollError == nil || sub.LastPollError.HTTPStatus != 429 || sub.NextPollAt.Before(retry) {
		t.Fatalf("%+v", sub)
	}
	if sub.NextAttemptAt == nil || !sub.NextAttemptAt.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("next_attempt_at must stay the next local scan: %v", sub.NextAttemptAt)
	}

	live.Store(true)
	for i := 0; i < 20; i++ {
		step(t, e, c, 30*time.Second)
	}
	sub, _ = s.GetAt("claude", c.Now())
	if sub.Error != nil || sub.Stale {
		t.Fatalf("fresh passive must clear the root error: %+v", sub)
	}
	if sub.LastPollError == nil || sub.NextPollAt.Before(retry) {
		t.Fatalf("new headers erased the 429 pause: %+v", sub)
	}
	hs, _ := s.Hybrid("claude")
	if hs.RemoteFailures != 1 {
		t.Fatalf("passive commits touched remote failures: %d", hs.RemoteFailures)
	}

	s.Close()
	h2 := &hfake{fetchFn: apiOK(c), obsFn: h.obsFn}
	e2, s2 := hsetup(t, dir, c, h2)
	if nb := e2.byID["claude"].scope.nb(); nb.Before(retry) {
		t.Fatalf("scope cooldown not restored: %v", nb)
	}
	live.Store(false)
	for i := 0; i < 200 && c.Now().Before(retry.Add(-30*time.Second)); i++ {
		step(t, e2, c, 30*time.Second)
	}
	if h2.fetches.Load() != 0 {
		t.Fatalf("fetched during restored pause: %d", h2.fetches.Load())
	}
	step(t, e2, c, 30*time.Second)
	if h2.fetches.Load() != 1 {
		t.Fatalf("not fetched after pause: %d", h2.fetches.Load())
	}
	if sub, _ = s2.GetAt("claude", c.Now()); sub.LastPollError != nil || sub.Error != nil {
		t.Fatalf("API success must clear the remote error: %+v", sub)
	}
}

func TestHybridRestoredScope429IgnoresClearedRootError(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s, _ := store.Open(dir, hdefs("claude"))
	retry := t0.Add(2 * time.Hour)
	err := s.Commit(model.Update{ID: "claude", AttemptAt: t0, NextAttemptAt: t0.Add(30 * time.Second),
		Result: ptrRes(passive(t0)),
		Hybrid: &model.HybridState{RemoteFailures: 1, NextRemoteAttemptAt: &retry,
			RemoteError: &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retry}}})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	h := &hfake{fetchFn: apiOK(c), obsFn: noObs}
	e, _ := hsetup(t, dir, c, h)
	if nb := e.byID["claude"].scope.nb(); nb.Before(retry) {
		t.Fatalf("not restored: %v", nb)
	}
	// local scans must not wait for the rate-limit window
	if w := e.initialWake(e.byID["claude"]); w.After(t0.Add(time.Minute)) {
		t.Fatalf("initial wake waits for scope 429: %v", w)
	}
}

func ptrRes(r model.Result) *model.Result { return &r }

func TestHybridSharedScopeBlocksOnlyRemote(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s, err := store.Open(dir, hdefs("claude", "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := &hfake{fetchFn: apiOK(c)}
	h.obsFn = func(context.Context, int) (model.Result, error) { return passive(c.Now().Add(-11 * time.Minute)), nil }
	other := &fake{fn: func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429}
	}}
	e, err := New(s, []Worker{
		{Spec: hspec("claude", "shared"), Collector: h},
		{Spec: spec("other", "shared", 5*time.Minute), Collector: other},
	}, opts(c))
	if err != nil {
		t.Fatal(err)
	}
	e.Poll(context.Background(), "other") // 429 raises the shared scope
	step(t, e, c, 30*time.Second)
	step(t, e, c, 30*time.Second)
	if h.fetches.Load() != 0 {
		t.Fatal("remote GET during shared scope cooldown")
	}
	if h.observes.Load() != 2 {
		t.Fatalf("observe must keep working: %d", h.observes.Load())
	}
	sub, _ := s.GetAt("claude", c.Now())
	if sub.LastPassiveAt == nil || sub.LastSuccessAt == nil {
		t.Fatalf("passive data lost: %+v", sub)
	}

	// a busy scope (sibling fetch in flight) also only blocks the remote
	c.Add(time.Hour)
	g := e.byID["claude"].scope
	g.sem <- struct{}{}
	c.Add(30 * time.Second)
	e.Poll(context.Background(), "claude")
	if h.fetches.Load() != 0 || h.observes.Load() != 3 {
		t.Fatalf("fetches %d observes %d", h.fetches.Load(), h.observes.Load())
	}
	<-g.sem
	step(t, e, c, 30*time.Second)
	if h.fetches.Load() != 1 {
		t.Fatalf("fetch after scope freed: %d", h.fetches.Load())
	}
}

func TestHybridExpiredNativeAuthWaitsThirtyMinutesWhilePassiveContinues(t *testing.T) {
	c := &clock{t: t0}
	h := &hfake{obsFn: noObs}
	h.fetchFn = func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "auth_expired", HTTPStatus: 401}
	}
	e, s := hsetup(t, t.TempDir(), c, h)
	e.Poll(context.Background(), "claude")
	sub, _ := s.GetAt("claude", c.Now())
	if sub.NextPollAt == nil || !sub.NextPollAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("auth retry %v", sub.NextPollAt)
	}
	for i := 0; i < 59; i++ {
		step(t, e, c, 30*time.Second)
	}
	if h.fetches.Load() != 1 {
		t.Fatalf("retried auth failure early: %d", h.fetches.Load())
	}
	step(t, e, c, 30*time.Second)
	if h.fetches.Load() != 2 {
		t.Fatalf("no retry after 30m: %d", h.fetches.Load())
	}
	if hs, _ := s.Hybrid("claude"); hs.RemoteFailures != 2 {
		t.Fatalf("%+v", hs)
	}
}

func TestHybridOldBridgeAfterAPISuccessDoesNotBackdateFreshness(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	var seen atomic.Pointer[time.Time]
	h := &hfake{fetchFn: apiOK(c)}
	h.obsFn = func(context.Context, int) (model.Result, error) {
		if at := seen.Load(); at != nil {
			return passive(*at), nil
		}
		return model.Result{}, errNoBridge
	}
	e, s := hsetup(t, dir, c, h)
	e.Poll(context.Background(), "claude") // API success observed at t0
	old := t0.Add(-15 * time.Minute)
	seen.Store(&old)
	step(t, e, c, 30*time.Second)
	sub, _ := s.GetAt("claude", c.Now())
	if !sub.LastSuccessAt.Equal(t0) || sub.Error != nil || sub.Stale {
		t.Fatalf("older bridge file replaced or poisoned a newer snapshot: %+v", sub)
	}
	if sub.LastPassiveAt == nil || !sub.LastPassiveAt.Equal(old) {
		t.Fatalf("passive clock %v", sub.LastPassiveAt)
	}
	if n := journal(t, dir, "attempts"); n != 1 {
		t.Fatalf("attempts %d", n)
	}
	// a bridge response only slightly older than the API snapshot still counts as recent activity
	r2 := t0.Add(-time.Minute)
	seen.Store(&r2)
	step(t, e, c, 30*time.Second)
	sub, _ = s.GetAt("claude", c.Now())
	if !sub.LastSuccessAt.Equal(t0) {
		t.Fatalf("backdated: %v", sub.LastSuccessAt)
	}
	if sub.NextPollAt == nil || !sub.NextPollAt.Equal(r2.Add(10*time.Minute)) {
		t.Fatalf("activity clock must advance: next poll %v", sub.NextPollAt)
	}
	c.Add(5 * time.Minute)
	e.Poll(context.Background(), "claude")
	if h.fetches.Load() != 1 {
		t.Fatalf("recent bridge did not suppress GET: %d", h.fetches.Load())
	}
}

func TestHybridUnchangedObservationLogsNothingNew(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	at := t0.Add(-time.Minute)
	h := &hfake{fetchFn: apiOK(c)}
	h.obsFn = func(context.Context, int) (model.Result, error) { return passive(at), nil }
	e, s := hsetup(t, dir, c, h)
	e.Poll(context.Background(), "claude")
	before, _ := s.GetAt("claude", c.Now())
	for i := 0; i < 10; i++ {
		step(t, e, c, 30*time.Second)
	}
	after, _ := s.GetAt("claude", c.Now())
	if journal(t, dir, "attempts") != 1 || journal(t, dir, "history") != 1 {
		t.Fatalf("duplicates: attempts %d history %d", journal(t, dir, "attempts"), journal(t, dir, "history"))
	}
	// metadata-only scans never freshen attempt/success
	if !after.LastAttemptAt.Equal(*before.LastAttemptAt) || !after.LastSuccessAt.Equal(*before.LastSuccessAt) {
		t.Fatalf("freshened: %+v", after)
	}
	if !after.NextAttemptAt.After(*before.NextAttemptAt) {
		t.Fatal("next scan not advanced")
	}
}

func TestHybridPassiveCommittedBeforeFailedRemote(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	old := t0.Add(-20 * time.Minute)
	h := &hfake{}
	h.obsFn = func(context.Context, int) (model.Result, error) { return passive(old), nil }
	h.fetchFn = func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "upstream_error", HTTPStatus: 500}
	}
	e, s := hsetup(t, dir, c, h)
	e.Poll(context.Background(), "claude")
	sub, _ := s.GetAt("claude", c.Now())
	if sub.LastSuccessAt == nil || !sub.LastSuccessAt.Equal(old) || len(sub.Quotas) != 1 {
		t.Fatalf("passive last-good lost: %+v", sub)
	}
	if sub.Error == nil || !sub.Stale || sub.LastPollError == nil {
		t.Fatalf("remote failure must mark root stale: %+v", sub)
	}
	if h.fetches.Load() != 1 || journal(t, dir, "attempts") != 2 || journal(t, dir, "history") != 1 {
		t.Fatalf("fetches %d attempts %d", h.fetches.Load(), journal(t, dir, "attempts"))
	}
	b, _ := os.ReadFile(filepath.Join(dir, "attempts", "claude.jsonl"))
	if i, j := strings.Index(string(b), `"outcome":"ok"`), strings.Index(string(b), `"outcome":"error"`); i < 0 || j < i {
		t.Fatalf("passive must be recorded before the failure: %s", b)
	}
	// repeated local scans (before the 1m backoff ends) add no phantom attempts
	step(t, e, c, 30*time.Second)
	if journal(t, dir, "attempts") != 2 || h.fetches.Load() != 1 {
		t.Fatal("scans logged attempts")
	}
}

func TestHybridRemoteBackoffIgnoresPassiveCommits(t *testing.T) {
	c := &clock{t: t0}
	var n atomic.Int32
	h := &hfake{}
	// every scan sees a *new* bridge file that is already older than the idle threshold
	h.obsFn = func(context.Context, int) (model.Result, error) {
		return passive(c.Now().Add(-11*time.Minute - time.Duration(n.Add(1))*time.Second)), nil
	}
	h.fetchFn = func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "upstream_error", HTTPStatus: 502}
	}
	e, s := hsetup(t, t.TempDir(), c, h)
	var waits []time.Duration
	for i := 0; i < 4; i++ {
		e.Poll(context.Background(), "claude")
		sub, _ := s.GetAt("claude", c.Now())
		waits = append(waits, sub.NextPollAt.Sub(c.Now()))
		c.t = *sub.NextPollAt
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits %v", waits)
		}
	}
	if hs, _ := s.Hybrid("claude"); hs.RemoteFailures != 4 {
		t.Fatalf("%+v", hs)
	}
}

func TestHybridFailureDoesNotUseFastLocalRetryAndHonorsRetryAfter(t *testing.T) {
	c := &clock{t: t0}
	deadline := t0.Add(3 * time.Hour)
	h := &hfake{obsFn: noObs}
	h.fetchFn = func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "upstream_error", HTTPStatus: 503, RetryAt: &deadline}
	}
	e, s := hsetup(t, t.TempDir(), c, h)
	e.Poll(context.Background(), "claude")
	sub, _ := s.GetAt("claude", c.Now())
	if sub.NextPollAt.Before(deadline) {
		t.Fatalf("503 Retry-After ignored: %v", sub.NextPollAt)
	}

	c2 := &clock{t: t0}
	h2 := &hfake{obsFn: noObs}
	h2.fetchFn = func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "upstream_error", HTTPStatus: 500}
	}
	e2, s2 := hsetup(t, t.TempDir(), c2, h2)
	e2.Poll(context.Background(), "claude")
	sub, _ = s2.GetAt("claude", c2.Now())
	if got := sub.NextPollAt.Sub(t0); got != time.Minute {
		t.Fatalf("hybrid inherited the passive-only fast retry: %v", got)
	}
	if got := sub.NextAttemptAt.Sub(t0); got != 30*time.Second {
		t.Fatalf("next scan %v", got)
	}
}

func TestHybridPersistFailureStillGuardsAgainstRapidGET(t *testing.T) {
	c := &clock{t: t0}
	var s *store.Store
	h := &hfake{obsFn: noObs}
	h.fetchFn = func(context.Context, int) (model.Result, error) {
		s.Close() // every later write fails
		return apiResult(c.Now()), nil
	}
	e, st := hsetup(t, t.TempDir(), c, h)
	s = st
	if _, err := e.Poll(context.Background(), "claude"); err == nil {
		t.Fatal("expected persistence error")
	}
	for i := 0; i < 8; i++ {
		c.Add(30 * time.Second)
		e.Poll(context.Background(), "claude")
	}
	if h.fetches.Load() != 1 {
		t.Fatalf("persistence failure allowed rapid GETs: %d", h.fetches.Load())
	}
}

func TestHybridCancelRecordsNothingForAbortedRemote(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	ctx, cancel := context.WithCancel(context.Background())
	h := &hfake{obsFn: noObs}
	h.fetchFn = func(ctx context.Context, _ int) (model.Result, error) {
		cancel()
		<-ctx.Done()
		return model.Result{}, &model.ErrorInfo{Code: "unauthorized", HTTPStatus: 401}
	}
	e, s := hsetup(t, dir, c, h)
	ok, err := e.Poll(ctx, "claude")
	if ok || err == nil {
		t.Fatal(ok, err)
	}
	sub, _ := s.GetAt("claude", c.Now())
	if sub.Error != nil || sub.LastAttemptAt != nil || sub.LastPollError != nil || journal(t, dir, "attempts") != 0 {
		t.Fatalf("cancellation recorded: %+v", sub)
	}
}

func TestHybridObserveErrorAloneNeverOverwritesGood(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	var bad atomic.Bool
	h := &hfake{fetchFn: apiOK(c)}
	h.obsFn = func(context.Context, int) (model.Result, error) {
		if bad.Load() {
			return model.Result{}, &model.ErrorInfo{Code: "invalid_response"}
		}
		return passive(c.Now()), nil
	}
	e, s := hsetup(t, dir, c, h)
	e.Poll(context.Background(), "claude")
	bad.Store(true)
	for i := 0; i < 5; i++ { // < idle: no remote either
		step(t, e, c, 30*time.Second)
	}
	sub, _ := s.GetAt("claude", c.Now())
	if sub.Error != nil || sub.Stale || journal(t, dir, "attempts") != 1 || h.fetches.Load() != 0 {
		t.Fatalf("phantom error or early GET: %+v", sub)
	}
}

func TestHybridOlderAPIResultIsMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	h := &hfake{obsFn: noObs}
	h.fetchFn = func(context.Context, int) (model.Result, error) { return apiResult(t0.Add(-time.Hour)), nil }
	s, _ := store.Open(dir, hdefs("claude"))
	defer s.Close()
	if err := s.Commit(model.Update{ID: "claude", AttemptAt: t0, NextAttemptAt: t0, Result: ptrRes(passive(t0.Add(-time.Minute)))}); err != nil {
		t.Fatal(err)
	}
	e, _ := New(s, []Worker{{Spec: hspec("claude", ""), Collector: h}}, opts(c))
	e.Poll(context.Background(), "claude")
	sub, _ := s.GetAt("claude", c.Now())
	if sub.Error != nil || sub.Stale || !sub.LastSuccessAt.Equal(t0.Add(-time.Minute)) {
		t.Fatalf("%+v", sub)
	}
	if sub.NextPollAt == nil || !sub.NextPollAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("remote schedule: %v", sub.NextPollAt)
	}
}

func TestHybridPlainPassiveCollectorKeepsLegacyBehavior(t *testing.T) {
	c := &clock{t: t0}
	f := &fake{fn: func(context.Context, int) (model.Result, error) { return model.Result{}, errNoBridge }}
	e, s := setup(t, t.TempDir(), c, f, "a")
	if e.byID["a"].hyb != nil {
		t.Fatal("plain collector treated as hybrid")
	}
	e.byID["a"].Spec.Source = collectors.SourceOpenCodeObserved
	e.byID["a"].Spec.Interval = 30 * time.Second
	e.Poll(context.Background(), "a")
	if sub, _ := s.Get("a"); sub.NextPollAt != nil || sub.LastPassiveAt != nil {
		t.Fatalf("%+v", sub)
	}
}

func TestHybridRunScansRepeatedlyAndStops(t *testing.T) {
	before := runtime.NumGoroutine()
	h := &hfake{}
	h.fetchFn = func(context.Context, int) (model.Result, error) { return apiResult(time.Now()), nil }
	h.obsFn = noObs
	s, _ := store.Open(t.TempDir(), hdefs("claude"))
	defer s.Close()
	sp := hspec("claude", "")
	sp.ObserveInterval = 20 * time.Millisecond
	e, err := New(s, []Worker{{Spec: sp, Collector: h}}, Options{BackoffBase: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- e.Run(ctx) }()
	for dl := time.Now().Add(5 * time.Second); h.observes.Load() < 5 && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if h.observes.Load() < 5 || h.fetches.Load() != 1 {
		t.Fatalf("observes %d fetches %d", h.observes.Load(), h.fetches.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
}
