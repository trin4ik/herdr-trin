package engine

import (
	"context"
	"errors"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

const profileInterval = 6 * time.Hour
const profileTimeout = 3 * time.Second

func (e *Engine) syncLocalProfile(ctx context.Context, w *worker) error {
	c, ok := w.Collector.(collectors.LocalProfileCollector)
	if !ok || ctx.Err() != nil {
		return nil
	}
	p, err := c.ReadLocalProfile(ctx)
	if err != nil || p == nil {
		// Missing metadata must not change the usage outcome or remote clocks.
		return e.st.VerifyProfileIdentity(w.id, "", false)
	}
	if err := e.st.CommitLocalProfile(w.id, *p); errors.Is(err, store.ErrIdentityChanged) {
		return nil
	} else {
		return err
	}
}

// pollProfile runs while the existing worker and rate-scope guards are held.
// HTTP clients only read store snapshots and never reach this method.
func (e *Engine) pollProfile(ctx context.Context, w *worker) error {
	c, ok := w.Collector.(collectors.ProfileCollector)
	if !ok {
		return nil
	}
	state, ok := e.st.RemoteProfile(w.id)
	if !ok || state.Identity == "" {
		return nil
	}
	now := e.opts.Now()
	if state.NextAttemptAt != nil && state.NextAttemptAt.After(now) {
		return nil
	}
	if w.scope.nb().After(now) || ctx.Err() != nil {
		return nil
	}
	timeout := profileTimeout
	if w.Spec.Timeout < timeout {
		timeout = w.Spec.Timeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var update model.ProfileUpdate
	var err error
	func() {
		defer func() {
			if recover() != nil {
				update = model.ProfileUpdate{}
				err = errors.New("profile collector panic")
			}
		}()
		update, err = c.FetchProfile(cctx, state.Identity)
	}()
	if ctx.Err() != nil {
		return nil
	}
	if err == nil && cctx.Err() != nil {
		err = context.DeadlineExceeded
	}
	if err == nil && (update.Profile == nil || !update.IdentityChecked || update.CurrentIdentity != state.Identity) {
		err = errors.New("unconfirmed profile response")
	}
	update.ExpectedIdentity = state.Identity
	update.AttemptAt = now
	end := e.opts.Now()
	if err == nil {
		update.Failure = nil
		update.NextAttemptAt = end.Add(e.successDelay(profileInterval))
	} else {
		update.Profile = nil
		update.Failure = classify(err)
		update.NextAttemptAt = e.failureNext(w, update.Failure, state.Failures, end)
		if floor := end.Add(5 * time.Minute); update.NextAttemptAt.Before(floor) {
			update.NextAttemptAt = floor
		}
		if update.Failure.HTTPStatus == 429 {
			if update.Failure.RetryAt == nil || update.NextAttemptAt.After(*update.Failure.RetryAt) {
				t := update.NextAttemptAt
				update.Failure.RetryAt = &t
			}
			w.scope.raise(update.NextAttemptAt)
		}
	}
	if err := e.st.CommitProfile(w.id, update); errors.Is(err, store.ErrOlderObservation) || errors.Is(err, store.ErrIdentityChanged) {
		return nil
	} else {
		return err
	}
}
