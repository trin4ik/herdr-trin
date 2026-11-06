package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/httpapi"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

// pfake is an offline keyed collector: usage reports the current key
// fingerprint, FetchProfile answers for it. Nothing here touches a network.
type pfake struct {
	c       *clock
	mu      sync.Mutex
	key     string
	usage   atomic.Int32
	profile atomic.Int32
	expect  atomic.Value // last ExpectedIdentity passed to FetchProfile
	fetchFn func(ctx context.Context, n int) (model.Result, error)
	profFn  func(ctx context.Context, expected string, n int) (model.ProfileUpdate, error)
	enter   func() func()
}

var _ collectors.ProfileCollector = (*pfake)(nil)

func newPF(c *clock, key string) *pfake { return &pfake{c: c, key: key} }

func (p *pfake) current() string { p.mu.Lock(); defer p.mu.Unlock(); return p.key }
func (p *pfake) setKey(k string) { p.mu.Lock(); p.key = k; p.mu.Unlock() }

func pEmailFor(key string) string { return key + "@example.test" }

func (p *pfake) Fetch(ctx context.Context) (model.Result, error) {
	n := int(p.usage.Add(1))
	if p.enter != nil {
		defer p.enter()()
	}
	if p.fetchFn != nil {
		return p.fetchFn(ctx, n)
	}
	r := okResult(p.c.Now())
	r.ProfileIdentity = p.current()
	return r, nil
}

func (p *pfake) okProfile(expected string) (model.ProfileUpdate, error) {
	cur := p.current()
	plan, email := "Plan "+expected, pEmailFor(expected)
	return model.ProfileUpdate{ExpectedIdentity: expected, CurrentIdentity: cur, IdentityChecked: true,
		Profile: &model.Profile{Identity: expected, AccountEmail: &email, Plan: &plan}}, nil
}

func (p *pfake) FetchProfile(ctx context.Context, expected string) (model.ProfileUpdate, error) {
	n := int(p.profile.Add(1))
	p.expect.Store(expected)
	if p.enter != nil {
		defer p.enter()()
	}
	if p.profFn != nil {
		return p.profFn(ctx, expected, n)
	}
	return p.okProfile(expected)
}

func pdefs(ids ...string) []model.Definition {
	var d []model.Definition
	for _, id := range ids {
		d = append(d, model.Definition{ID: id, Provider: "ollama", Product: "p", Source: "s", StaleAfter: time.Hour, PublishAccountEmail: true})
	}
	return d
}

func pw(id, scope string, interval time.Duration, f collectors.Collector, mut ...func(*collectors.Spec)) Worker {
	if scope == "" {
		scope = id
	}
	sp := collectors.Spec{Definition: model.Definition{ID: id, Provider: "ollama"}, Interval: interval, Timeout: time.Second, Scope: scope}
	for _, m := range mut {
		m(&sp)
	}
	return Worker{Spec: sp, Collector: f}
}

func pengine(t *testing.T, dir string, c *clock, ws ...Worker) (*Engine, *store.Store) {
	t.Helper()
	var ids []string
	for _, w := range ws {
		ids = append(ids, w.Spec.ID)
	}
	s, err := store.Open(dir, pdefs(ids...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e, err := New(s, ws, opts(c))
	if err != nil {
		t.Fatal(err)
	}
	return e, s
}

func pPoll(t *testing.T, e *Engine, id string, want bool) {
	t.Helper()
	ok, err := e.Poll(context.Background(), id)
	if err != nil || ok != want {
		t.Fatalf("Poll(%s) = %v, %v; want %v", id, ok, err, want)
	}
}

func pSched(t *testing.T, s *store.Store, id string) model.ProfileState {
	t.Helper()
	st, ok := s.RemoteProfileSchedule(id)
	if !ok {
		t.Fatalf("no profile state for %s", id)
	}
	return st
}

func pEmail(t *testing.T, s *store.Store, id string) *string {
	t.Helper()
	sub, ok := s.Get(id)
	if !ok {
		t.Fatalf("missing %s", id)
	}
	return sub.AccountEmail
}

func pWantEmail(t *testing.T, s *store.Store, id, key string) {
	t.Helper()
	if e := pEmail(t, s, id); e == nil || *e != pEmailFor(key) {
		t.Fatalf("%s email = %v, want %s", id, e, pEmailFor(key))
	}
}

// pUsageUntouched asserts that a profile-only event left the usage record of a
// single successful poll at t0 (interval) exactly as the usage commit made it.
func pUsageUntouched(t *testing.T, s *store.Store, id string, interval time.Duration) {
	t.Helper()
	sub, _ := s.Get(id)
	if sub.Error != nil || sub.LastSuccessAt == nil || !sub.LastSuccessAt.Equal(t0) ||
		sub.LastAttemptAt == nil || !sub.LastAttemptAt.Equal(t0) ||
		sub.NextAttemptAt == nil || !sub.NextAttemptAt.Equal(t0.Add(interval)) || s.Failures(id) != 0 {
		t.Fatalf("usage state changed by profile: %+v failures=%d", sub, s.Failures(id))
	}
}

func TestProfileRunsAfterUsageCommitAndIsCachedSixHours(t *testing.T) {
	c := &clock{t: t0}
	f := newPF(c, "key-a")
	var s *store.Store
	var usageCommitted atomic.Bool
	f.profFn = func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
		sub, _ := s.Get("a")
		usageCommitted.Store(sub.LastSuccessAt != nil && sub.LastAttemptAt != nil)
		return f.okProfile(exp)
	}
	var e *Engine
	e, s = pengine(t, t.TempDir(), c, pw("a", "", 10*time.Minute, f))

	pPoll(t, e, "a", true)
	if !usageCommitted.Load() || f.usage.Load() != 1 || f.profile.Load() != 1 {
		t.Fatalf("usage=%d profile=%d committedFirst=%v", f.usage.Load(), f.profile.Load(), usageCommitted.Load())
	}
	pWantEmail(t, s, "a", "key-a")
	if sub, _ := s.Get("a"); sub.Plan == nil || *sub.Plan != "Plan key-a" {
		t.Fatalf("plan %v", sub.Plan)
	}
	st := pSched(t, s, "a")
	if st.LastAttemptAt == nil || !st.LastAttemptAt.Equal(t0) || st.NextAttemptAt == nil || !st.NextAttemptAt.Equal(t0.Add(6*time.Hour)) || st.Failures != 0 {
		t.Fatalf("schedule %+v", st)
	}
	pUsageUntouched(t, s, "a", 10*time.Minute)

	c.Add(10 * time.Minute)
	pPoll(t, e, "a", true)
	c.Add(5*time.Hour + 49*time.Minute) // 5h59m
	pPoll(t, e, "a", true)
	if f.usage.Load() != 3 || f.profile.Load() != 1 {
		t.Fatalf("profile refetched inside its 6h cache: usage=%d profile=%d", f.usage.Load(), f.profile.Load())
	}
	c.Add(10 * time.Minute) // 6h09m
	pPoll(t, e, "a", true)
	if f.profile.Load() != 2 {
		t.Fatalf("profile not refreshed after 6h: %d", f.profile.Load())
	}
	if st := pSched(t, s, "a"); !st.NextAttemptAt.Equal(t0.Add(12*time.Hour + 9*time.Minute)) {
		t.Fatalf("next %v", st.NextAttemptAt)
	}
}

func TestProfileSkippedWithoutIdentityOrOnUsageFailure(t *testing.T) {
	c := &clock{t: t0}
	f := newPF(c, "")
	f.fetchFn = func(context.Context, int) (model.Result, error) { return okResult(c.Now()), nil } // no ProfileIdentity
	e, s := pengine(t, t.TempDir(), c, pw("a", "", 10*time.Minute, f))
	pPoll(t, e, "a", true)
	if f.profile.Load() != 0 {
		t.Fatal("profile requested without a verified credential generation")
	}
	if _, ok := s.RemoteProfileSchedule("a"); ok {
		t.Fatal("profile state invented")
	}

	// usage failure: no profile request, cached metadata stays when the key is re-proved
	g := newPF(c, "key-a")
	e2, s2 := pengine(t, t.TempDir(), c, pw("b", "", 10*time.Minute, g))
	pPoll(t, e2, "b", true)
	g.fetchFn = func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "http_500", HTTPStatus: 500, ProfileIdentity: "key-a", ProfileIdentityChecked: true}
	}
	c.Add(time.Hour)
	pPoll(t, e2, "b", true)
	if g.profile.Load() != 1 {
		t.Fatalf("profile requested after failed usage: %d", g.profile.Load())
	}
	pWantEmail(t, s2, "b", "key-a")
	if sub, _ := s2.Get("b"); sub.Error == nil || sub.Error.Code != "http_500" {
		t.Fatalf("root error %+v", sub.Error)
	}
	// a failure that proves a different key hides the old key's metadata
	g.fetchFn = func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "http_500", HTTPStatus: 500, ProfileIdentity: "key-b", ProfileIdentityChecked: true}
	}
	c.Add(time.Hour)
	pPoll(t, e2, "b", true)
	if pEmail(t, s2, "b") != nil || g.profile.Load() != 1 {
		t.Fatalf("old key metadata visible after switch: %v", pEmail(t, s2, "b"))
	}
}

func TestProfileTimeoutDoesNotTouchUsage(t *testing.T) {
	for _, name := range []string{"blocks until cancelled", "ignores cancellation and answers late"} {
		t.Run(name, func(t *testing.T) {
			c := &clock{t: t0}
			f := newPF(c, "key-a")
			if strings.HasPrefix(name, "blocks") {
				f.profFn = func(ctx context.Context, _ string, _ int) (model.ProfileUpdate, error) {
					<-ctx.Done()
					return model.ProfileUpdate{}, ctx.Err()
				}
			} else {
				f.profFn = func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
					time.Sleep(80 * time.Millisecond)
					return f.okProfile(exp)
				}
			}
			e, s := pengine(t, t.TempDir(), c, pw("a", "", 10*time.Minute, f, func(sp *collectors.Spec) { sp.Timeout = 30 * time.Millisecond }))
			pPoll(t, e, "a", true)
			st := pSched(t, s, "a")
			if st.Error == nil || st.Error.Code != "timeout" || st.Failures != 1 || !st.NextAttemptAt.Equal(t0.Add(5*time.Minute)) {
				t.Fatalf("profile timeout state %+v err=%+v", st, st.Error)
			}
			if pEmail(t, s, "a") != nil {
				t.Fatal("late answer must not be trusted")
			}
			pUsageUntouched(t, s, "a", 10*time.Minute)
			if sub, _ := s.Get("a"); len(sub.Quotas) == 0 {
				t.Fatal("usage quotas lost")
			}
		})
	}
}

func TestProfileDeadlineIsThreeSecondsOrSpecTimeout(t *testing.T) {
	for _, tc := range []struct {
		spec     time.Duration
		min, max time.Duration
	}{
		{time.Minute, 2 * time.Second, 3 * time.Second},
		{500 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond},
	} {
		c := &clock{t: t0}
		f := newPF(c, "key-a")
		var left atomic.Int64
		f.profFn = func(ctx context.Context, exp string, _ int) (model.ProfileUpdate, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Error("profile request has no deadline")
			}
			left.Store(int64(time.Until(dl)))
			return f.okProfile(exp)
		}
		e, _ := pengine(t, t.TempDir(), c, pw("a", "", 10*time.Minute, f, func(sp *collectors.Spec) { sp.Timeout = tc.spec }))
		pPoll(t, e, "a", true)
		if d := time.Duration(left.Load()); d <= tc.min || d > tc.max {
			t.Fatalf("spec timeout %v: profile deadline %v away, want (%v, %v]", tc.spec, d, tc.min, tc.max)
		}
	}
}

func TestProfileFailureClassesUseSeparateCooldownNotUsageErrors(t *testing.T) {
	retry := t0.Add(9 * time.Hour) // beyond the 6h success cache
	cases := []struct {
		name  string
		err   error
		code  string
		after time.Duration
	}{
		{"server error floors at 5m", &model.ErrorInfo{Code: "http_500", HTTPStatus: 500}, "http_500", 5 * time.Minute},
		{"timeout", context.DeadlineExceeded, "timeout", 5 * time.Minute},
		{"unknown error is fixed code", errors.New("secret detail " + "key-a"), store.CodeCollectionError, 5 * time.Minute},
		{"auth uses the slow retry", &model.ErrorInfo{Code: "unauthorized", HTTPStatus: 401}, "unauthorized", 2 * time.Hour},
		{"503 Retry-After beyond 6h", &model.ErrorInfo{Code: "http_503", HTTPStatus: 503, RetryAt: &retry}, "http_503", 9 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: t0}
			f := newPF(c, "key-a")
			f.profFn = func(_ context.Context, exp string, n int) (model.ProfileUpdate, error) {
				if n > 1 {
					return f.okProfile(exp)
				}
				return model.ProfileUpdate{ExpectedIdentity: exp, CurrentIdentity: exp, IdentityChecked: true}, tc.err
			}
			e, s := pengine(t, t.TempDir(), c, pw("a", "", time.Minute, f))
			pPoll(t, e, "a", true)
			st := pSched(t, s, "a")
			if st.Error == nil || st.Error.Code != tc.code || st.Failures != 1 || !st.NextAttemptAt.Equal(t0.Add(tc.after)) {
				t.Fatalf("state %+v error %+v, want %s at +%v", st, st.Error, tc.code, tc.after)
			}
			pUsageUntouched(t, s, "a", time.Minute)

			// usage keeps running every minute; the profile waits for its own cooldown
			c.Add(tc.after - time.Second)
			pPoll(t, e, "a", true)
			if f.profile.Load() != 1 {
				t.Fatal("profile cooldown bypassed by a later usage success")
			}
			if sub, _ := s.Get("a"); sub.Error != nil {
				t.Fatalf("profile error leaked into usage error: %+v", sub.Error)
			}
			c.Add(time.Minute)
			pPoll(t, e, "a", true)
			if f.profile.Load() != 2 || pSched(t, s, "a").Error != nil {
				t.Fatalf("profile not retried when due: calls=%d", f.profile.Load())
			}
			pWantEmail(t, s, "a", "key-a")
		})
	}
}

func TestProfileFailureBackoffGrowsIndependentlyOfUsage(t *testing.T) {
	c := &clock{t: t0}
	f := newPF(c, "key-a")
	f.profFn = func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
		return model.ProfileUpdate{ExpectedIdentity: exp, CurrentIdentity: exp, IdentityChecked: true}, &model.ErrorInfo{Code: "http_500", HTTPStatus: 500}
	}
	e, s := pengine(t, t.TempDir(), c, pw("a", "", time.Minute, f, func(sp *collectors.Spec) { sp.BackoffBase = 4 * time.Minute }))
	pPoll(t, e, "a", true)
	if st := pSched(t, s, "a"); !st.NextAttemptAt.Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("first %v", st.NextAttemptAt)
	}
	c.Add(5 * time.Minute)
	pPoll(t, e, "a", true)
	st := pSched(t, s, "a")
	if st.Failures != 2 || !st.NextAttemptAt.Equal(t0.Add(13*time.Minute)) {
		t.Fatalf("second failure: %+v", st)
	}
	if s.Failures("a") != 0 {
		t.Fatal("profile failures counted as usage failures")
	}
}

func TestProfile429CooldownBeyondCacheRestoresSharedScopeAfterRestart(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	fa, fb := newPF(c, "key-a"), newPF(c, "key-b")
	retry := t0.Add(9 * time.Hour)
	fa.profFn = func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
		return model.ProfileUpdate{ExpectedIdentity: exp, CurrentIdentity: exp, IdentityChecked: true},
			&model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retry}
	}
	mk := func() (*Engine, *store.Store) {
		return pengine(t, dir, c, pw("a", "shared", time.Minute, fa), pw("b", "shared", time.Minute, fb))
	}
	e, s := mk()
	pPoll(t, e, "a", true)
	st := pSched(t, s, "a")
	if st.Error == nil || st.Error.HTTPStatus != 429 || !st.NextAttemptAt.Equal(retry) || st.Error.RetryAt == nil || !st.Error.RetryAt.Equal(retry) {
		t.Fatalf("429 state %+v %+v", st, st.Error)
	}
	pUsageUntouched(t, s, "a", time.Minute)
	pPoll(t, e, "b", false) // the sibling in the same upstream scope waits too
	c.Add(7 * time.Hour)
	pPoll(t, e, "a", false)
	pPoll(t, e, "b", false)
	s.Close()

	e2, s2 := mk()
	if st := pSched(t, s2, "a"); !st.NextAttemptAt.Equal(retry) || st.Error == nil || st.Failures != 1 {
		t.Fatalf("restart shortened/lost the cooldown: %+v", st)
	}
	if pEmail(t, s2, "a") != nil {
		t.Fatal("cached metadata visible before usage revalidation")
	}
	pPoll(t, e2, "a", false)
	pPoll(t, e2, "b", false)
	c.Add(2*time.Hour - time.Second)
	pPoll(t, e2, "b", false)
	c.Add(time.Second)
	pPoll(t, e2, "b", true)
	pPoll(t, e2, "a", true)
	if fa.usage.Load() != 2 || fb.usage.Load() != 1 || fa.profile.Load() != 2 {
		t.Fatalf("usage a=%d b=%d profile a=%d", fa.usage.Load(), fb.usage.Load(), fa.profile.Load())
	}
}

func TestProfile503RetryAfterBeyondCacheSurvivesRestartWithoutBlockingUsage(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	f := newPF(c, "key-a")
	retry := t0.Add(9 * time.Hour)
	f.profFn = func(_ context.Context, exp string, n int) (model.ProfileUpdate, error) {
		if n > 1 {
			return f.okProfile(exp)
		}
		return model.ProfileUpdate{ExpectedIdentity: exp, CurrentIdentity: exp, IdentityChecked: true},
			&model.ErrorInfo{Code: "http_503", HTTPStatus: 503, RetryAt: &retry}
	}
	mk := func() (*Engine, *store.Store) { return pengine(t, dir, c, pw("a", "", time.Minute, f)) }
	e, s := mk()
	pPoll(t, e, "a", true)
	s.Close()
	e2, s2 := mk()
	c.Add(7 * time.Hour)
	pPoll(t, e2, "a", true) // usage is not held back by a profile-only 503
	if f.usage.Load() != 2 || f.profile.Load() != 1 {
		t.Fatalf("usage=%d profile=%d", f.usage.Load(), f.profile.Load())
	}
	if !pSched(t, s2, "a").NextAttemptAt.Equal(retry) {
		t.Fatal("cooldown shortened")
	}
	c.Add(2 * time.Hour)
	pPoll(t, e2, "a", true)
	if f.profile.Load() != 2 {
		t.Fatalf("profile not retried after Retry-After: %d", f.profile.Load())
	}
	pWantEmail(t, s2, "a", "key-a")
}

func TestNewHidesCachedProfileUntilUsageRevalidatesKey(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	f := newPF(c, "key-a")
	e, s := pengine(t, dir, c, pw("a", "", 10*time.Minute, f))
	pPoll(t, e, "a", true)
	pWantEmail(t, s, "a", "key-a")
	s.Close()

	// same key: hidden at startup, revealed by the first usage success without a new profile request
	e2, s2 := pengine(t, dir, c, pw("a", "", 10*time.Minute, f))
	if pEmail(t, s2, "a") != nil {
		t.Fatal("restored metadata served before the key was revalidated")
	}
	if sub, _ := s2.Get("a"); sub.Plan != nil {
		t.Fatal("restored plan served before revalidation")
	}
	b, _ := json.Marshal(s2.Snapshot(c.Now()))
	if strings.Contains(string(b), "example.test") || strings.Contains(string(b), "Plan key-a") {
		t.Fatalf("snapshot leaks restored metadata: %s", b)
	}
	c.Add(10 * time.Minute)
	pPoll(t, e2, "a", true)
	pWantEmail(t, s2, "a", "key-a")
	if f.profile.Load() != 1 {
		t.Fatalf("cached profile refetched: %d", f.profile.Load())
	}
	s2.Close()

	// usage that cannot verify the key keeps it hidden and does not request metadata
	f.fetchFn = func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "http_500", HTTPStatus: 500}
	}
	e3, s3 := pengine(t, dir, c, pw("a", "", 10*time.Minute, f))
	c.Add(10 * time.Minute)
	pPoll(t, e3, "a", true)
	if pEmail(t, s3, "a") != nil || f.profile.Load() != 1 {
		t.Fatal("unverified key revealed metadata")
	}
	s3.Close()

	// key switched while the service was down: old metadata never appears, new one is fetched at once
	f.fetchFn = nil
	f.setKey("key-b")
	e4, s4 := pengine(t, dir, c, pw("a", "", 10*time.Minute, f))
	c.Add(10 * time.Minute)
	pPoll(t, e4, "a", true)
	pWantEmail(t, s4, "a", "key-b")
	if got, _ := f.expect.Load().(string); got != "key-b" || f.profile.Load() != 2 {
		t.Fatalf("profile fetched for %q, calls %d", got, f.profile.Load())
	}
	b, _ = json.Marshal(s4.Snapshot(c.Now()))
	if strings.Contains(string(b), "key-a") {
		t.Fatalf("old key metadata leaked: %s", b)
	}
}

func TestKeySwitchKeepsFailureCooldownButNotSuccessTTL(t *testing.T) {
	c := &clock{t: t0}
	f := newPF(c, "key-a")
	e, s := pengine(t, t.TempDir(), c, pw("a", "", time.Minute, f))
	pPoll(t, e, "a", true) // success cached until +6h
	f.setKey("key-b")
	c.Add(time.Minute)
	pPoll(t, e, "a", true)
	pWantEmail(t, s, "a", "key-b")
	if f.profile.Load() != 2 {
		t.Fatalf("new credential must not wait for the old success TTL: %d", f.profile.Load())
	}

	f.profFn = func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
		return model.ProfileUpdate{ExpectedIdentity: exp, CurrentIdentity: exp, IdentityChecked: true},
			&model.ErrorInfo{Code: "http_500", HTTPStatus: 500}
	}
	f.setKey("key-c")
	c.Add(time.Minute)
	pPoll(t, e, "a", true) // c: fails, 5m cooldown
	f.setKey("key-d")
	c.Add(time.Minute)
	pPoll(t, e, "a", true)
	if f.profile.Load() != 3 || pEmail(t, s, "a") != nil {
		t.Fatalf("failure cooldown must carry over a key switch: calls=%d", f.profile.Load())
	}
	f.profFn = nil
	c.Add(5 * time.Minute)
	pPoll(t, e, "a", true)
	pWantEmail(t, s, "a", "key-d")
}

func TestProfileMidflightKeySwitchRejectsStaleResponse(t *testing.T) {
	c := &clock{t: t0}
	f := newPF(c, "key-a")
	var s *store.Store
	f.profFn = func(_ context.Context, exp string, n int) (model.ProfileUpdate, error) {
		if n == 1 {
			// the key changes and a usage poll commits it while the request is in flight
			f.setKey("key-b")
			r := okResult(c.Now())
			r.ProfileIdentity = "key-b"
			if err := s.Commit(model.Update{ID: "a", AttemptAt: c.Now(), NextAttemptAt: c.Now().Add(10 * time.Minute), Result: &r}); err != nil {
				t.Error(err)
			}
		}
		// the stale response still carries the old account's data
		cur := exp
		upd, err := f.okProfile(exp)
		upd.CurrentIdentity = cur
		return upd, err
	}
	var e *Engine
	e, s = pengine(t, t.TempDir(), c, pw("a", "", 10*time.Minute, f))
	pPoll(t, e, "a", true)
	if pEmail(t, s, "a") != nil {
		t.Fatalf("old key's metadata applied to the new key")
	}
	st := pSched(t, s, "a")
	if st.Identity != "key-b" || st.NextAttemptAt != nil || st.Error != nil {
		t.Fatalf("new generation not left fetchable: %+v", st)
	}
	c.Add(10 * time.Minute)
	pPoll(t, e, "a", true)
	pWantEmail(t, s, "a", "key-b")
	if got, _ := f.expect.Load().(string); got != "key-b" {
		t.Fatalf("expected %q", got)
	}
}

func TestProfileCollectorDetectedSwitchNeverLeaksOldMetadata(t *testing.T) {
	c := &clock{t: t0}
	f := newPF(c, "key-a")
	e, s := pengine(t, t.TempDir(), c, pw("a", "", 10*time.Minute, f))
	pPoll(t, e, "a", true)
	pWantEmail(t, s, "a", "key-a")

	f.profFn = func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
		f.setKey("key-b")
		return model.ProfileUpdate{ExpectedIdentity: exp, CurrentIdentity: "key-b", IdentityChecked: true},
			&model.ErrorInfo{Code: "profile_identity_changed"}
	}
	c.Add(6*time.Hour + time.Minute)
	pPoll(t, e, "a", true)
	if pEmail(t, s, "a") != nil {
		t.Fatal("metadata of the previous key still served")
	}
	if sub, _ := s.Get("a"); sub.Error != nil || sub.Plan != nil {
		t.Fatalf("%+v", sub)
	}
	f.profFn = nil
	c.Add(time.Hour)
	pPoll(t, e, "a", true)
	pWantEmail(t, s, "a", "key-b")
}

func TestUnconfirmedOrBrokenProfileResponsesAreFailures(t *testing.T) {
	cases := map[string]func(*pfake) func(context.Context, string, int) (model.ProfileUpdate, error){
		"panic": func(*pfake) func(context.Context, string, int) (model.ProfileUpdate, error) {
			return func(context.Context, string, int) (model.ProfileUpdate, error) { panic("boom key-a") }
		},
		"nil profile without error": func(*pfake) func(context.Context, string, int) (model.ProfileUpdate, error) {
			return func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
				return model.ProfileUpdate{ExpectedIdentity: exp, CurrentIdentity: exp, IdentityChecked: true}, nil
			}
		},
		"identity not checked": func(f *pfake) func(context.Context, string, int) (model.ProfileUpdate, error) {
			return func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
				u, err := f.okProfile(exp)
				u.IdentityChecked = false
				return u, err
			}
		},
		"different current identity": func(f *pfake) func(context.Context, string, int) (model.ProfileUpdate, error) {
			return func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
				u, err := f.okProfile(exp)
				u.CurrentIdentity = ""
				return u, err
			}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			c := &clock{t: t0}
			f := newPF(c, "key-a")
			f.profFn = mk(f)
			e, s := pengine(t, t.TempDir(), c, pw("a", "", 10*time.Minute, f))
			pPoll(t, e, "a", true)
			st := pSched(t, s, "a")
			if st.Error == nil || st.Error.Code != store.CodeCollectionError || st.Failures != 1 {
				t.Fatalf("%+v %+v", st, st.Error)
			}
			if pEmail(t, s, "a") != nil {
				t.Fatal("unconfirmed metadata published")
			}
			pUsageUntouched(t, s, "a", 10*time.Minute)
		})
	}
}

func TestCancelledProfileRecordsNothing(t *testing.T) {
	c := &clock{t: t0}
	f := newPF(c, "key-a")
	ctx, cancel := context.WithCancel(context.Background())
	f.profFn = func(ctx context.Context, _ string, _ int) (model.ProfileUpdate, error) {
		cancel()
		<-ctx.Done()
		return model.ProfileUpdate{}, ctx.Err()
	}
	e, s := pengine(t, t.TempDir(), c, pw("a", "", 10*time.Minute, f))
	if _, err := e.Poll(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	st := pSched(t, s, "a")
	if st.LastAttemptAt != nil || st.Error != nil || st.Failures != 0 || st.NextAttemptAt != nil {
		t.Fatalf("shutdown recorded as a profile failure: %+v", st)
	}
	pUsageUntouched(t, s, "a", 10*time.Minute)
}

func TestTwoAccountsOfOneProviderKeepProfilesAndCooldownsApart(t *testing.T) {
	c := &clock{t: t0}
	f1, f2 := newPF(c, "key-1"), newPF(c, "key-2")
	retry := t0.Add(9 * time.Hour)
	f1.profFn = func(_ context.Context, exp string, n int) (model.ProfileUpdate, error) {
		if n == 1 {
			return f1.okProfile(exp)
		}
		return model.ProfileUpdate{ExpectedIdentity: exp, CurrentIdentity: exp, IdentityChecked: true},
			&model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retry}
	}
	e, s := pengine(t, t.TempDir(), c, pw("o1", "", 10*time.Minute, f1), pw("o2", "", 10*time.Minute, f2))
	pPoll(t, e, "o1", true)
	pPoll(t, e, "o2", true)
	pWantEmail(t, s, "o1", "key-1")
	pWantEmail(t, s, "o2", "key-2")

	f1.setKey("key-3")
	c.Add(10 * time.Minute)
	pPoll(t, e, "o1", true) // 429 for the new key of account 1
	pPoll(t, e, "o2", true) // separate upstream scope: not blocked, own cache untouched
	if pEmail(t, s, "o1") != nil {
		t.Fatal("o1 shows the previous key's metadata")
	}
	pWantEmail(t, s, "o2", "key-2")
	if f2.profile.Load() != 1 || f2.usage.Load() != 2 {
		t.Fatalf("o2 usage=%d profile=%d", f2.usage.Load(), f2.profile.Load())
	}
	if st := pSched(t, s, "o2"); st.Error != nil || st.Identity != "key-2" {
		t.Fatalf("o2 inherited o1 state: %+v", st)
	}
	b, _ := json.Marshal(s.Snapshot(c.Now()))
	if strings.Contains(string(b), "key-1@") || strings.Contains(string(b), "key-3@") || !strings.Contains(string(b), "key-2@example.test") {
		t.Fatalf("%s", b)
	}
}

func TestSharedScopeNeverOverlapsUsageAndProfileRequests(t *testing.T) {
	c := &clock{t: t0}
	var cur, max atomic.Int32
	enter := func() func() {
		n := cur.Add(1)
		for {
			m := max.Load()
			if n <= m || max.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		return func() { cur.Add(-1) }
	}
	f1, f2 := newPF(c, "key-1"), newPF(c, "key-2")
	f1.enter, f2.enter = enter, enter
	e, s := pengine(t, t.TempDir(), c, pw("o1", "shared", 10*time.Minute, f1), pw("o2", "shared", 10*time.Minute, f2))
	var wg sync.WaitGroup
	for _, id := range []string{"o1", "o2"} {
		wg.Add(1)
		go func() { defer wg.Done(); e.Poll(context.Background(), id) }()
	}
	wg.Wait()
	if max.Load() != 1 || f1.profile.Load() != 1 || f2.profile.Load() != 1 {
		t.Fatalf("max concurrent %d, profiles %d/%d", max.Load(), f1.profile.Load(), f2.profile.Load())
	}
	pWantEmail(t, s, "o1", "key-1")
	pWantEmail(t, s, "o2", "key-2")
}

func TestConcurrentPollsAndHTTPReadsDeduplicateProviderTraffic(t *testing.T) {
	c := &clock{t: t0}
	f := newPF(c, "key-a")
	usageIn, profileIn := make(chan struct{}), make(chan struct{})
	releaseUsage, releaseProfile := make(chan struct{}), make(chan struct{})
	var once1, once2 sync.Once
	f.fetchFn = func(context.Context, int) (model.Result, error) {
		once1.Do(func() { close(usageIn) })
		<-releaseUsage
		r := okResult(c.Now())
		r.ProfileIdentity = f.current()
		return r, nil
	}
	f.profFn = func(_ context.Context, exp string, _ int) (model.ProfileUpdate, error) {
		once2.Do(func() { close(profileIn) })
		<-releaseProfile
		return f.okProfile(exp)
	}
	e, s := pengine(t, t.TempDir(), c, pw("a", "", 10*time.Minute, f))
	h := httpapi.NewUnauthenticated(s, c.Now)
	get := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/quotas", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET status %d", rec.Code)
		}
		return rec.Body.String()
	}

	var started atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Add(1)
			if _, err := e.Poll(context.Background(), "a"); err != nil {
				t.Error(err)
			}
		}()
	}
	<-usageIn
	for i := 0; i < 20; i++ {
		get()
	}
	close(releaseUsage)
	<-profileIn
	for i := 0; i < 20; i++ {
		if body := get(); strings.Contains(body, "example.test") {
			t.Fatalf("metadata visible before the profile commit: %s", body)
		}
		pPoll(t, e, "a", false)
	}
	close(releaseProfile)
	wg.Wait()

	for i := 0; i < 50; i++ {
		if body := get(); !strings.Contains(body, `"account_email":"key-a@example.test"`) {
			t.Fatalf("metadata not served: %s", body)
		}
	}
	pPoll(t, e, "a", false) // not due
	if f.usage.Load() != 1 || f.profile.Load() != 1 {
		t.Fatalf("provider traffic: usage=%d profile=%d", f.usage.Load(), f.profile.Load())
	}
}

func TestProfileWorkerWithoutProfileCollectorIsUnchanged(t *testing.T) {
	c := &clock{t: t0}
	f := &fake{fn: func(context.Context, int) (model.Result, error) {
		r := okResult(c.Now())
		r.ProfileIdentity = "key-a" // ignored by the engine: no ProfileCollector, no profile requests
		return r, nil
	}}
	s, err := store.Open(t.TempDir(), pdefs("a"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e, err := New(s, []Worker{pw("a", "", 10*time.Minute, f)}, opts(c))
	if err != nil {
		t.Fatal(err)
	}
	pPoll(t, e, "a", true)
	if sub, _ := s.Get("a"); sub.Error != nil || sub.AccountEmail != nil {
		t.Fatalf("%+v", sub)
	}
}
