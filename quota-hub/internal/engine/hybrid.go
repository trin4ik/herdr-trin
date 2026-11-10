package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

// Hybrid workers (collectors.HybridCollector) run two independent clocks:
//
//   - a local scan (Spec.ObserveInterval, default 30s) that calls the network-free
//     Observe and is never delayed by a rate-limit cooldown or a busy scope;
//   - a remote clock (HybridState.NextRemoteAttemptAt) that allows one upstream
//     Fetch per subscription interval, as every other provider does.
//
// Both clocks report windows of the same subscription. The collector merges them:
// a window only the remote source knows (a model-scoped weekly limit, for
// example) does not disappear when a passive observation arrives, and the newer
// value of a window both sources report wins.
//
// The stored next_attempt_at of a hybrid worker is the next local scan; the remote
// deadline is exposed as next_poll_at.
const (
	defaultObserveEvery = 30 * time.Second
)

type hybridRuntime struct {
	hy   collectors.HybridCollector
	scan time.Duration

	mu    sync.Mutex
	guard time.Time // in-memory remote not-before: survives a failed persist
}

func (h *hybridRuntime) guardUntil() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.guard
}

func (h *hybridRuntime) raiseGuard(t time.Time) {
	h.mu.Lock()
	if t.After(h.guard) {
		h.guard = t
	}
	h.mu.Unlock()
}

func newHybridRuntime(w Worker) *hybridRuntime {
	hy, ok := w.Collector.(collectors.HybridCollector)
	if !ok {
		return nil
	}
	h := &hybridRuntime{hy: hy, scan: w.Spec.ObserveInterval}
	if h.scan <= 0 {
		h.scan = defaultObserveEvery
	}
	return h
}

// restoreHybridScope re-arms a shared-scope 429 from the persisted remote error.
// The root error cannot be used: a later passive success clears it.
func restoreHybridScope(g *scopeGuard, hs model.HybridState) {
	re := hs.RemoteError
	if re == nil || !(re.HTTPStatus == 429 || re.Code == "rate_limited") {
		return
	}
	if hs.NextRemoteAttemptAt != nil {
		g.raise(*hs.NextRemoteAttemptAt)
	}
	if re.RetryAt != nil {
		g.raise(*re.RetryAt)
	}
}

// raiseForPassive keeps the remote deadline untouched: a passive observation is
// local data and never postpones the remote request. The remote clock follows
// the subscription interval like every other provider.
func (w *worker) raiseForPassive(hs *model.HybridState, now time.Time) {}

func (w *worker) remoteAllowed(hs model.HybridState, now time.Time) bool {
	if hs.NextRemoteAttemptAt != nil && hs.NextRemoteAttemptAt.After(now) {
		return false
	}
	return !w.hyb.guardUntil().After(now) && !w.scope.nb().After(now)
}

// Expose/persist the same remote floor used by the shared-scope gate, while
// leaving local observation scheduling untouched.
func (w *worker) raiseRemoteFloor(hs *model.HybridState, now time.Time) {
	for _, floor := range []time.Time{w.scope.nb(), w.hyb.guardUntil()} {
		if floor.After(now) && (hs.NextRemoteAttemptAt == nil || floor.After(*hs.NextRemoteAttemptAt)) {
			v := floor
			hs.NextRemoteAttemptAt = &v
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }

func (e *Engine) pollHybrid(ctx context.Context, w *worker) (bool, time.Time, error) {
	now := e.opts.Now()
	sub, ok := e.st.Get(w.id)
	if !ok {
		return false, now, ErrUnknownWorker
	}
	if sub.NextAttemptAt != nil && sub.NextAttemptAt.After(now) {
		return false, *sub.NextAttemptAt, nil
	}
	if err := ctx.Err(); err != nil {
		return false, now, err
	}
	hs, _ := e.st.Hybrid(w.id)
	w.raiseRemoteFloor(&hs, now)
	scanNext := now.Add(w.hyb.scan)

	// 1. local observation: runs on every scan, whatever the remote state is
	persisted := false
	obs, oerr := e.guarded(ctx, w, w.hyb.hy.Observe)
	if ctx.Err() != nil {
		return false, now, ctx.Err()
	}
	// An Observe error alone never touches last-good; the remote side decides.
	if oerr == nil && !obs.ObservedAt.IsZero() {
		p := obs.ObservedAt.UTC()
		activity := p
		if activity.After(now) {
			activity = now
		}
		if hs.LastPassiveAt == nil || activity.After(*hs.LastPassiveAt) {
			hs.LastPassiveAt = &activity
		}
		if sub.LastSuccessAt == nil || p.After(*sub.LastSuccessAt) {
			w.raiseForPassive(&hs, now)
			h := hs
			err := e.st.Commit(model.Update{ID: w.id, AttemptAt: now, NextAttemptAt: scanNext, Result: &obs, Hybrid: &h})
			if errors.Is(err, store.ErrOlderObservation) || errors.Is(err, store.ErrIdentityChanged) {
				err = nil
			}
			if err != nil {
				return true, scanNext, err
			}
			persisted = true
		}
	}
	w.raiseForPassive(&hs, now)
	w.raiseRemoteFloor(&hs, now)

	// 2. remote GET only when idle, past every cooldown and the scope is free
	if w.remoteAllowed(hs, now) {
		select {
		case w.scope.sem <- struct{}{}:
			defer func() { <-w.scope.sem }()
			if !w.scope.nb().After(now) {
				return e.remoteHybrid(ctx, w, hs, now)
			}
		default:
		}
	}
	if persisted {
		return true, scanNext, nil
	}
	return true, scanNext, e.st.SetHybrid(w.id, hs, scanNext)
}

// remoteHybrid performs exactly one upstream Fetch and records it. Caller holds the scope.
func (e *Engine) remoteHybrid(ctx context.Context, w *worker, hs model.HybridState, now time.Time) (bool, time.Time, error) {
	res, ferr := e.fetch(ctx, w)
	if ctx.Err() != nil {
		return false, now, ctx.Err() // shutdown: record nothing for the aborted remote
	}
	end := e.opts.Now()
	scanNext := end.Add(w.hyb.scan)

	out := hs
	out.LastRemoteAttemptAt = ptr(now)
	upd := model.Update{ID: w.id, AttemptAt: now, NextAttemptAt: scanNext}
	if ferr == nil {
		out.RemoteFailures, out.RemoteError = 0, nil
		out.NextRemoteAttemptAt = ptr(end.Add(e.successDelay(w.Spec.Interval)))
		w.raiseForPassive(&out, end)
		w.hyb.raiseGuard(*out.NextRemoteAttemptAt)
		cur, _ := e.st.Get(w.id)
		if cur.LastSuccessAt != nil && res.ObservedAt.Before(*cur.LastSuccessAt) {
			// never replace a newer snapshot with an older one, nor record a bogus error
			return true, scanNext, e.st.SetHybrid(w.id, out, scanNext)
		}
		upd.Result = &res
	} else {
		info := classify(ferr)
		next := e.failureNext(w, info, hs.RemoteFailures, end)
		if info.HTTPStatus == 429 {
			if info.RetryAt == nil || next.After(*info.RetryAt) {
				info.RetryAt = ptr(next)
			}
			w.scope.raise(next)
		}
		ei := *info
		out.RemoteFailures = hs.RemoteFailures + 1
		out.RemoteError = &ei
		out.NextRemoteAttemptAt = ptr(next)
		w.raiseForPassive(&out, end)
		w.hyb.raiseGuard(*out.NextRemoteAttemptAt)
		upd.Failure = info
	}
	upd.Hybrid = &out
	err := e.st.Commit(upd)
	if errors.Is(err, store.ErrOlderObservation) {
		err = nil
	}
	return true, scanNext, err
}
