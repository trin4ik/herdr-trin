package engine

import (
	"context"
	"testing"
	"time"

	"quota-hub/internal/collectors"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

func TestNativeCredentialErrorsUseSlowAuthPolicy(t *testing.T) {
	for _, code := range []string{"auth_required", "access_denied", "credentials_unavailable"} {
		t.Run(code, func(t *testing.T) {
			c := &clock{t: t0}
			f := &fake{fn: func(context.Context, int) (model.Result, error) {
				return model.Result{}, &model.ErrorInfo{Code: code}
			}}
			e, s := setup(t, t.TempDir(), c, f, "a")
			if _, err := e.Poll(context.Background(), "a"); err != nil {
				t.Fatal(err)
			}
			sub, _ := s.Get("a")
			if !sub.NextAttemptAt.Equal(t0.Add(2 * time.Hour)) {
				t.Fatal("local credential failure used rapid transient retries")
			}
		})
	}
}

func TestServiceUnavailableRetryAfterIsAlsoRespected(t *testing.T) {
	c := &clock{t: t0}
	deadline := t0.Add(48 * time.Hour)
	f := &fake{fn: func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "upstream_error", HTTPStatus: 503, RetryAt: &deadline}
	}}
	e, s := setup(t, t.TempDir(), c, f, "a")
	if _, err := e.Poll(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	sub, _ := s.Get("a")
	if sub.NextAttemptAt.Before(deadline) {
		t.Fatal("503 Retry-After ignored")
	}
}

func TestMissingPassiveBridgeUsesLocalFileInterval(t *testing.T) {
	c := &clock{t: t0}
	f := &fake{fn: func(context.Context, int) (model.Result, error) {
		return model.Result{}, &model.ErrorInfo{Code: "source_unavailable"}
	}}
	e, s := setup(t, t.TempDir(), c, f, "a")
	e.byID["a"].Spec.Source = collectors.SourceOpenCodeObserved
	e.byID["a"].Spec.Interval = 30 * time.Second
	if _, err := e.Poll(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	sub, _ := s.Get("a")
	if !sub.NextAttemptAt.Equal(t0.Add(30 * time.Second)) {
		t.Fatal("missing passive file inherited remote API backoff")
	}
}

func TestCadenceReductionReschedulesSuccessButNeverFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "rate_limited"}[failed], func(t *testing.T) {
			c := &clock{t: t0.Add(6 * time.Minute)}
			f := &fake{fn: func(context.Context, int) (model.Result, error) { return okResult(c.Now()), nil }}
			s, err := store.Open(t.TempDir(), defsFor("a"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			result := okResult(t0)
			deadline := t0.Add(time.Hour)
			update := model.Update{ID: "a", AttemptAt: t0, NextAttemptAt: t0.Add(15 * time.Minute), Result: &result}
			if failed {
				update.Result = nil
				update.NextAttemptAt = deadline
				update.Failure = &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &deadline}
			}
			if err := s.Commit(update); err != nil {
				t.Fatal(err)
			}
			w := spec("a", "a", 5*time.Minute)
			e, err := New(s, []Worker{{Spec: w, Collector: f}}, opts(c))
			if err != nil {
				t.Fatal(err)
			}
			attempted, err := e.Poll(context.Background(), "a")
			if err != nil || attempted == failed {
				t.Fatal("new cadence failed to distinguish healthy schedule from server pause")
			}
			if failed {
				sub, _ := s.Get("a")
				if !sub.NextAttemptAt.Equal(deadline) || f.calls.Load() != 0 {
					t.Fatal("interval change bypassed a persisted 429")
				}
			}
		})
	}
}
