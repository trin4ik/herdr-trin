// Package engine schedules read-only collectors and records every attempt in the store.
//
// Failure classification (from the collector's error):
//   - *model.ErrorInfo (via errors.As) with HTTPStatus 429 or code "rate_limited":
//     wait max(exponential backoff, RetryAt); a server date is never shortened.
//   - HTTPStatus 401/403 or code unauthorized/forbidden/auth_failed/auth_expired/
//     token_expired: slow AuthRetry. The engine never refreshes credentials.
//   - everything else (5xx, network, timeout, unknown): exponential backoff with jitter.
//
// Hybrid collectors (see hybrid.go) keep these rules for their remote GET but have
// no fast local-file retry: their remote failures are counted in the persisted
// hybrid state, independently of passive observations.
//
// Errors that are not *model.ErrorInfo are never copied into state: they become the
// fixed codes "timeout", "network_error" or "collection_error".
package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

var (
	ErrUnknownWorker = errors.New("engine: unknown worker")
	ErrRunning       = errors.New("engine: already running")
)

const (
	defaultTimeout = 30 * time.Second
	minSpin        = 50 * time.Millisecond
)

type Worker struct {
	Spec      collectors.Spec
	Collector collectors.Collector
}

// Options: Now defaults to time.Now. Jitter is a fraction (0 disables, clamped to
// 0.5). BackoffBase/BackoffMax bound transient retries; AuthRetry is the slow
// retry for 401/403.
type Options struct {
	Now         func() time.Time
	Jitter      float64
	BackoffBase time.Duration
	BackoffMax  time.Duration
	AuthRetry   time.Duration
}

type scopeGuard struct {
	sem       chan struct{}
	mu        sync.Mutex
	notBefore time.Time
}

func (g *scopeGuard) nb() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.notBefore
}

func (g *scopeGuard) raise(t time.Time) {
	g.mu.Lock()
	if t.After(g.notBefore) {
		g.notBefore = t
	}
	g.mu.Unlock()
}

type worker struct {
	Worker
	id      string
	scope   *scopeGuard
	running atomic.Bool
	hyb     *hybridRuntime // non-nil for collectors.HybridCollector workers

	// per-provider policy: Spec fields when > 0, otherwise Options
	base, max, auth, pause time.Duration
}

type Engine struct {
	st      *store.Store
	opts    Options
	workers []*worker
	byID    map[string]*worker
	active  atomic.Bool

	rndMu sync.Mutex
	rnd   *rand.Rand
}

func pick(spec, fallback time.Duration) time.Duration {
	if spec > 0 {
		return spec
	}
	return fallback
}

func New(s *store.Store, workers []Worker, opts Options) (*Engine, error) {
	if s == nil {
		return nil, errors.New("engine: nil store")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Jitter < 0 {
		opts.Jitter = 0
	}
	if opts.Jitter > 0.5 {
		opts.Jitter = 0.5
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = 30 * time.Second
	}
	if opts.BackoffMax < opts.BackoffBase {
		opts.BackoffMax = 15 * time.Minute
		if opts.BackoffMax < opts.BackoffBase {
			opts.BackoffMax = opts.BackoffBase
		}
	}
	if opts.AuthRetry <= 0 {
		opts.AuthRetry = time.Hour
	}
	e := &Engine{st: s, opts: opts, byID: map[string]*worker{},
		rnd: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9e3779b97f4a7c15))}
	scopes := map[string]*scopeGuard{}
	for _, w := range workers {
		id := w.Spec.ID
		if w.Collector == nil || !store.ValidID(id) {
			return nil, fmt.Errorf("engine: invalid worker %q", id)
		}
		if _, dup := e.byID[id]; dup {
			return nil, fmt.Errorf("engine: duplicate worker %q", id)
		}
		sub, ok := s.Get(id)
		if !ok {
			return nil, fmt.Errorf("engine: worker %q not defined in store", id)
		}
		if w.Spec.Interval <= 0 {
			return nil, fmt.Errorf("engine: worker %q has no interval", id)
		}
		if w.Spec.Timeout <= 0 {
			w.Spec.Timeout = defaultTimeout
		}
		scope := w.Spec.Scope
		if scope == "" {
			scope = w.Spec.Provider
		}
		g := scopes[scope]
		if g == nil {
			g = &scopeGuard{sem: make(chan struct{}, 1)}
			scopes[scope] = g
		}
		if _, ok := w.Collector.(collectors.ProfileCollector); ok {
			if ps, found := s.RemoteProfileSchedule(id); found && ps.Error != nil && ps.Error.HTTPStatus == 429 {
				if ps.NextAttemptAt != nil {
					g.raise(*ps.NextAttemptAt)
				}
				if ps.Error.RetryAt != nil {
					g.raise(*ps.Error.RetryAt)
				}
			}
			if err := s.HideRemoteProfile(id); err != nil {
				return nil, err
			}
		}
		hyb := newHybridRuntime(w)
		// a restored 429 cooldown applies to the whole shared scope
		if hyb != nil {
			if hs, ok := s.Hybrid(id); ok {
				restoreHybridScope(g, hs)
			}
		} else if sub.Error != nil && sub.Error.HTTPStatus == 429 {
			if sub.NextAttemptAt != nil {
				g.raise(*sub.NextAttemptAt)
			}
			if sub.Error.RetryAt != nil {
				g.raise(*sub.Error.RetryAt)
			}
		}
		wk := &worker{Worker: w, id: id, scope: g, hyb: hyb,
			base: pick(w.Spec.BackoffBase, opts.BackoffBase),
			max:  pick(w.Spec.BackoffMax, opts.BackoffMax),
			auth: pick(w.Spec.AuthRetry, opts.AuthRetry),
		}
		if wk.max < wk.base {
			wk.max = wk.base
		}
		wk.pause = w.Spec.RateLimitPause // 0: derived per attempt, see failureNext
		if hyb == nil && w.Spec.Source != collectors.SourceOpenCodeObserved && sub.Error == nil && sub.LastSuccessAt != nil && sub.NextAttemptAt != nil {
			// An interval reduction should not leave one old fifteen-minute wait.
			// Only healthy successful schedules are shortened; failures and all
			// shared-scope gates remain untouched.
			upper := sub.LastSuccessAt.Add(time.Duration(float64(w.Spec.Interval)*(1+opts.Jitter)) + w.Spec.Timeout)
			if sub.NextAttemptAt.After(upper) {
				next := sub.LastSuccessAt.Add(e.successDelay(w.Spec.Interval))
				if now := opts.Now(); next.Before(now) {
					next = now
				}
				if err := s.RescheduleSuccess(id, next); err != nil {
					return nil, err
				}
			}
		}
		e.workers = append(e.workers, wk)
		e.byID[id] = wk
		if err := e.verifyProfile(wk); err != nil {
			return nil, err
		}
		if err := e.syncLocalProfile(context.Background(), wk); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// Run starts one goroutine per worker and blocks until ctx is cancelled and all
// goroutines have exited. It returns nil on cancellation.
func (e *Engine) Run(ctx context.Context) error {
	if !e.active.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer e.active.Store(false)
	var wg sync.WaitGroup
	for _, w := range e.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.loop(ctx, w)
		}()
	}
	wg.Wait()
	return nil
}

func (e *Engine) loop(ctx context.Context, w *worker) {
	wake := e.initialWake(w)
	for {
		if !sleepUntil(ctx, e.opts.Now, wake) {
			return
		}
		attempted, next, err := e.pollOnce(ctx, w)
		if ctx.Err() != nil {
			return
		}
		wake = next
		if !attempted || err != nil {
			// not due, busy, or persistence trouble: never spin
			if floor := e.opts.Now().Add(minSpin); !wake.After(floor) {
				wake = floor
			}
			if err != nil {
				if back := e.opts.Now().Add(w.base); back.After(wake) {
					wake = back
				}
			}
		}
	}
}

func (e *Engine) initialWake(w *worker) time.Time {
	now := e.opts.Now()
	wake := now
	if sub, ok := e.st.Get(w.id); ok && sub.NextAttemptAt != nil && sub.NextAttemptAt.After(now) {
		wake = *sub.NextAttemptAt
	}
	spread := w.Spec.Interval
	if w.hyb != nil {
		// local scans are never held back by a remote cooldown
		spread = w.hyb.scan
	} else if nb := w.scope.nb(); nb.After(wake) {
		wake = nb
	}
	// stagger first polls so startup is not a burst
	if spread > time.Minute {
		spread = time.Minute
	}
	return wake.Add(time.Duration(e.rand() * e.opts.Jitter * float64(spread)))
}

func sleepUntil(ctx context.Context, now func() time.Time, t time.Time) bool {
	d := t.Sub(now())
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Poll runs one attempt for id if it is due (stored NextAttemptAt, shared scope
// cooldown) and no other attempt for the worker is in flight. It reports whether
// an attempt was made. The error is a commit/persistence error or ctx error;
// collector failures are recorded, not returned.
func (e *Engine) Poll(ctx context.Context, id string) (bool, error) {
	w := e.byID[id]
	if w == nil {
		return false, ErrUnknownWorker
	}
	ok, _, err := e.pollOnce(ctx, w)
	return ok, err
}

func (e *Engine) pollOnce(ctx context.Context, w *worker) (attempted bool, next time.Time, err error) {
	if !w.running.CompareAndSwap(false, true) {
		return false, e.opts.Now().Add(minSpin), nil
	}
	defer w.running.Store(false)
	if err := e.verifyProfile(w); err != nil {
		return false, e.opts.Now(), err
	}
	if err := e.syncLocalProfile(ctx, w); err != nil {
		return false, e.opts.Now(), err
	}

	if w.hyb != nil {
		return e.pollHybrid(ctx, w)
	}

	select {
	case w.scope.sem <- struct{}{}:
	case <-ctx.Done():
		return false, time.Time{}, ctx.Err()
	}
	defer func() { <-w.scope.sem }()

	now := e.opts.Now()
	sub, ok := e.st.Get(w.id)
	if !ok {
		return false, now, ErrUnknownWorker
	}
	due := now
	if sub.NextAttemptAt != nil && sub.NextAttemptAt.After(due) {
		due = *sub.NextAttemptAt
	}
	if nb := w.scope.nb(); nb.After(due) {
		due = nb
	}
	if due.After(now) {
		return false, due, nil
	}
	if err := ctx.Err(); err != nil {
		return false, now, err
	}

	res, ferr := e.fetch(ctx, w)
	if ctx.Err() != nil {
		return false, now, ctx.Err() // shutdown: record nothing
	}
	end := e.opts.Now()
	upd := model.Update{ID: w.id, AttemptAt: now}
	if ferr == nil {
		upd.Result = &res
		upd.NextAttemptAt = end.Add(e.successDelay(w.Spec.Interval))
	} else {
		info := classify(ferr)
		failures := e.st.Failures(w.id)
		upd.NextAttemptAt = e.failureNext(w, info, failures, end)
		upd.Failure = info
		if info.HTTPStatus == 429 {
			if info.RetryAt == nil || upd.NextAttemptAt.After(*info.RetryAt) {
				t := upd.NextAttemptAt
				info.RetryAt = &t
			}
			w.scope.raise(upd.NextAttemptAt)
		}
	}
	err = e.st.Commit(upd)
	if errors.Is(err, store.ErrOlderObservation) {
		err = nil
	} else if err == nil && ferr == nil {
		// Metadata has its own timeout/error state. Never make an optional
		// profile request part of the usage success/failure transaction.
		err = e.pollProfile(ctx, w)
	}
	return true, upd.NextAttemptAt, err
}

func (e *Engine) verifyProfile(w *worker) error {
	if p, ok := w.Collector.(collectors.ProfileIdentity); ok {
		id, err := p.MetadataIdentity()
		return e.st.VerifyProfileIdentity(w.id, id, err == nil && id != "")
	}
	return nil
}

func (e *Engine) fetch(ctx context.Context, w *worker) (model.Result, error) {
	res, err := e.guarded(ctx, w, w.Collector.Fetch)
	// A credential switch while a request was in flight must hide metadata even
	// when that request failed. Persistence errors propagate, never publish PII.
	if p, ok := w.Collector.(collectors.ProfileIdentity); ok {
		id, checkErr := p.MetadataIdentity()
		valid := checkErr == nil && id != ""
		if verifyErr := e.st.VerifyProfileIdentity(w.id, id, valid); verifyErr != nil {
			return model.Result{}, verifyErr
		}
		if !valid {
			res.Profile = nil
		}
	}
	return res, err
}

// guarded runs one collector call with the worker timeout and panic recovery.
func (e *Engine) guarded(ctx context.Context, w *worker, call func(context.Context) (model.Result, error)) (res model.Result, err error) {
	cctx, cancel := context.WithTimeout(ctx, w.Spec.Timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			res, err = model.Result{}, errors.New("collector panic")
		}
	}()
	res, err = call(cctx)
	if err == nil && cctx.Err() != nil && ctx.Err() == nil {
		// timed out while returning data: do not trust a late answer
		return model.Result{}, context.DeadlineExceeded
	}
	return res, err
}

func isAuth(i *model.ErrorInfo) bool {
	switch i.HTTPStatus {
	case 401, 403:
		return true
	}
	switch i.Code {
	case "unauthorized", "forbidden", "auth_failed", "auth_expired", "token_expired",
		"auth_required", "access_denied", "credentials_unavailable":
		return true
	}
	return false
}

func classify(err error) *model.ErrorInfo {
	var info *model.ErrorInfo
	if errors.As(err, &info) && info != nil {
		c := *info
		if c.HTTPStatus == 0 && c.Code == "rate_limited" {
			c.HTTPStatus = 429
		}
		return &c
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &model.ErrorInfo{Code: "timeout"}
	case errors.As(err, &ne):
		return &model.ErrorInfo{Code: "network_error"}
	}
	return &model.ErrorInfo{Code: store.CodeCollectionError}
}

func (e *Engine) rand() float64 {
	e.rndMu.Lock()
	defer e.rndMu.Unlock()
	return e.rnd.Float64()
}

// spread returns d scaled by a factor in [1-J, 1+J].
func (e *Engine) spread(d time.Duration) time.Duration {
	if e.opts.Jitter == 0 {
		return d
	}
	return time.Duration(float64(d) * (1 + e.opts.Jitter*(2*e.rand()-1)))
}

func (e *Engine) successDelay(interval time.Duration) time.Duration {
	return e.spread(interval)
}

func (e *Engine) backoff(w *worker, failures int) time.Duration {
	d := w.base
	for i := 0; i < failures && d < w.max; i++ {
		d *= 2
	}
	d = e.spread(d)
	if d > w.max {
		d = w.max
	}
	return d
}

// failureNext computes the absolute next attempt time.
//   - 429: wait at least max(RateLimitPause, exponential backoff); without a
//     configured pause the floor is max(Interval, backoff). Jitter only adds.
//     A server RetryAt is a hard floor and is never shortened or clamped.
//   - 401/403: AuthRetry plus positive jitter.
//   - other: exponential backoff with symmetric jitter, capped at the worker's max.
func (e *Engine) failureNext(w *worker, info *model.ErrorInfo, failures int, end time.Time) time.Time {
	if w.hyb == nil && w.Spec.Source == collectors.SourceOpenCodeObserved {
		// This only checks a local file. A bridge enabled later should become visible
		// promptly instead of inheriting an hour-long network-failure backoff.
		return end.Add(e.successDelay(w.Spec.Interval))
	}
	switch {
	case info.HTTPStatus == 429:
		floor := w.base
		for i := 0; i < failures && floor < w.max; i++ {
			floor *= 2
		}
		if floor > w.max {
			floor = w.max
		}
		wait := w.pause
		if wait <= 0 {
			wait = w.Spec.Interval
		}
		if floor > wait {
			wait = floor
		}
		next := end.Add(wait + time.Duration(e.rand()*e.opts.Jitter*float64(wait)))
		if info.RetryAt != nil {
			srv := *info.RetryAt
			jittered := srv.Add(time.Duration(e.rand() * e.opts.Jitter * float64(w.base)))
			if jittered.Year() <= 9999 {
				srv = jittered
			}
			if srv.After(next) {
				next = srv
			}
		}
		return next
	case isAuth(info):
		return end.Add(w.auth + time.Duration(e.rand()*e.opts.Jitter*float64(w.auth)))
	}
	next := end.Add(e.backoff(w, failures))
	// Retry-After also applies to service-unavailable responses (e.g. HTTP 503).
	if info.RetryAt != nil && info.RetryAt.After(next) {
		next = *info.RetryAt
	}
	return next
}
