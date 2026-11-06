package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quota-hub/internal/model"
)

func tp(t time.Time) *time.Time { return &t }

func hybridFixture() model.HybridState {
	return model.HybridState{
		LastPassiveAt:       tp(t0.Add(-time.Minute)),
		LastRemoteAttemptAt: tp(t0),
		NextRemoteAttemptAt: tp(t0.Add(time.Hour)),
		RemoteFailures:      2,
		RemoteError:         &model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: tp(t0.Add(time.Hour))},
	}
}

func stateVersionOnDisk(t *testing.T, dir, id string) int {
	t.Helper()
	var st struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "state", id+".json"))), &st); err != nil {
		t.Fatal(err)
	}
	return st.Version
}

func TestSetHybridIsMetadataOnlyAndDoesNotFreshen(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	obs := t0.Add(-time.Minute)
	if err := s.Commit(okUpdate("claude", t0, result(obs, 40))); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetAt("claude", t0)
	hist := lines(t, filepath.Join(dir, "history", "claude.jsonl"))
	att := lines(t, filepath.Join(dir, "attempts", "claude.jsonl"))
	failures := s.Failures("claude")

	next := t0.Add(30 * time.Second)
	if err := s.SetHybrid("claude", hybridFixture(), next); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetAt("claude", t0)
	if !after.LastSuccessAt.Equal(*before.LastSuccessAt) || !after.LastAttemptAt.Equal(*before.LastAttemptAt) {
		t.Fatalf("metadata write freshened: %+v", after)
	}
	if !after.NextAttemptAt.Equal(next) {
		t.Fatalf("next %v", after.NextAttemptAt)
	}
	if after.Error != nil || after.Stale || s.Failures("claude") != failures {
		t.Fatalf("error/stale/failures changed: %+v", after)
	}
	if got := lines(t, filepath.Join(dir, "history", "claude.jsonl")); len(got) != len(hist) {
		t.Fatal("history invented")
	}
	if got := lines(t, filepath.Join(dir, "attempts", "claude.jsonl")); len(got) != len(att) {
		t.Fatal("attempt invented")
	}
	if after.LastPassiveAt == nil || !after.LastPassiveAt.Equal(obs) ||
		after.NextPollAt == nil || !after.NextPollAt.Equal(t0.Add(time.Hour)) ||
		after.LastPollError == nil || after.LastPollError.HTTPStatus != 429 {
		t.Fatalf("view hybrid fields: %+v", after)
	}
	if v := stateVersionOnDisk(t, dir, "claude"); v != stateVersionHybrid {
		t.Fatalf("version %d", v)
	}
}

func TestSetHybridOnEmptyStateKeepsUnknown(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	if err := s.SetHybrid("claude", hybridFixture(), t0); err != nil {
		t.Fatal(err)
	}
	sub, _ := s.GetAt("claude", t0)
	if sub.Status != "unknown" || sub.LastAttemptAt != nil || sub.LastSuccessAt != nil || len(sub.Quotas) != 0 {
		t.Fatalf("%+v", sub)
	}
	if err := s.SetHybrid("nope", hybridFixture(), t0); !errors.Is(err, ErrUnknownID) {
		t.Fatal(err)
	}
	if err := s.SetHybrid("claude", hybridFixture(), time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestHybridPersistsAcrossReopenAndIsDeepCloned(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, ok := s.Hybrid("claude"); ok {
		t.Fatal("hybrid before any write")
	}
	if err := s.SetHybrid("claude", hybridFixture(), t0); err != nil {
		t.Fatal(err)
	}
	h, ok := s.Hybrid("claude")
	if !ok {
		t.Fatal("missing")
	}
	*h.NextRemoteAttemptAt = t0.Add(99 * time.Hour)
	h.RemoteError.Code = "mutated"
	*h.RemoteError.RetryAt = t0
	if again, _ := s.Hybrid("claude"); !again.NextRemoteAttemptAt.Equal(t0.Add(time.Hour)) ||
		again.RemoteError.Code != "rate_limited" || !again.RemoteError.RetryAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("clone shares memory: %+v", again)
	}
	s.Close()

	s = open(t, dir)
	defer s.Close()
	got, ok := s.Hybrid("claude")
	if !ok || got.RemoteFailures != 2 || !got.NextRemoteAttemptAt.Equal(t0.Add(time.Hour)) || got.RemoteError == nil {
		t.Fatalf("not restored: %+v", got)
	}
}

func TestCommitPersistsHybridAtomicallyAndPlainCommitKeepsIt(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	hs := hybridFixture()
	u := failUpdate("claude", t0, "rate_limited")
	u.Hybrid = &hs
	if err := s.Commit(u); err != nil {
		t.Fatal(err)
	}
	if v := stateVersionOnDisk(t, dir, "claude"); v != stateVersionHybrid {
		t.Fatalf("version %d", v)
	}
	// a passive success clears the root error, the remote error and deadline stay
	if err := s.Commit(okUpdate("claude", t0.Add(time.Minute), result(t0.Add(time.Minute), 10))); err != nil {
		t.Fatal(err)
	}
	sub, _ := s.GetAt("claude", t0.Add(time.Minute))
	if sub.Error != nil || sub.Status != "ok" {
		t.Fatalf("root error not cleared: %+v", sub)
	}
	if sub.LastPollError == nil || sub.LastPollError.HTTPStatus != 429 || sub.NextPollAt == nil ||
		!sub.NextPollAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("remote pause lost: %+v", sub)
	}
	s.Close()
	s = open(t, dir)
	defer s.Close()
	if h, ok := s.Hybrid("claude"); !ok || h.RemoteError == nil || h.RemoteFailures != 2 {
		t.Fatalf("lost on reopen: %+v", h)
	}
	if got := lines(t, filepath.Join(dir, "attempts", "claude.jsonl")); len(got) != 2 {
		t.Fatalf("attempts %d", len(got))
	}
}

func TestHybridStateIsSanitized(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	hs := model.HybridState{RemoteFailures: -4,
		RemoteError: &model.ErrorInfo{Code: "Bad Code https://x?token=SECRET", HTTPStatus: 429}}
	if err := s.SetHybrid("claude", hs, t0); err != nil {
		t.Fatal(err)
	}
	h, _ := s.Hybrid("claude")
	if h.RemoteFailures != 0 || h.RemoteError.Code != CodeCollectionError {
		t.Fatalf("%+v", h)
	}
}

func TestStateVersionGuard(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.Commit(okUpdate("codex", t0, result(t0, 25))); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHybrid("claude", hybridFixture(), t0); err != nil {
		t.Fatal(err)
	}
	if v := stateVersionOnDisk(t, dir, "codex"); v != stateVersion {
		t.Fatalf("regular state must remain v1, got %d", v)
	}
	s.Close()

	// a v2 file without hybrid metadata is not something we write: fail closed
	path := filepath.Join(dir, "state", "codex.json")
	b := readFile(t, path)
	if err := os.WriteFile(path, []byte(strings.Replace(b, `"version":1`, `"version":2`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := Open(dir, defs()); !errors.Is(err, ErrCorrupt) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("v2 without hybrid accepted: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(b, `"version":1`, `"version":3`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := Open(dir, defs()); !errors.Is(err, ErrCorrupt) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("unknown version accepted: %v", err)
	}
}

func TestActualSourceOverrideKeepsDefinitionIdentity(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	r := result(t0, 30)
	r.Source = "claude_oauth_usage"
	if err := s.Commit(okUpdate("claude", t0, r)); err != nil {
		t.Fatal(err)
	}
	sub, _ := s.GetAt("claude", t0)
	if sub.Source != "claude_oauth_usage" {
		t.Fatalf("view source %q", sub.Source)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "history", "claude.jsonl")), `"source":"claude_oauth_usage"`) ||
		!strings.Contains(readFile(t, filepath.Join(dir, "attempts", "claude.jsonl")), `"source":"claude_oauth_usage"`) {
		t.Fatal("journal lost actual source")
	}
	// next result without override reverts to the definition source
	r2 := result(t0.Add(time.Minute), 31)
	if err := s.Commit(okUpdate("claude", t0.Add(time.Minute), r2)); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetAt("claude", t0.Add(time.Minute)); sub.Source != "claude_code" {
		t.Fatalf("definition source expected, got %q", sub.Source)
	}
	r3 := result(t0.Add(2*time.Minute), 32)
	r3.Source = "bad source\n{\"x\":1}"
	if err := s.Commit(okUpdate("claude", t0.Add(2*time.Minute), r3)); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetAt("claude", t0.Add(2*time.Minute)); sub.Source != "claude_code" {
		t.Fatalf("unsafe source accepted: %q", sub.Source)
	}
	r4 := result(t0.Add(3*time.Minute), 33)
	r4.Source = "opencode_observed"
	if err := s.Commit(okUpdate("claude", t0.Add(3*time.Minute), r4)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// the definition hash ignores overrides: the same definition reopens fine,
	// and the persisted override is restored
	s = open(t, dir)
	defer s.Close()
	if sub, _ = s.GetAt("claude", t0.Add(3*time.Minute)); sub.Source != "opencode_observed" {
		t.Fatalf("override not restored: %q", sub.Source)
	}
	if b := readFile(t, filepath.Join(dir, "state", "claude.json")); !strings.Contains(b, `"source":"opencode_observed"`) {
		t.Fatal("source not persisted in private good")
	}
}

func TestOlderObservationStillKeepsHybridAndGood(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	if err := s.Commit(okUpdate("claude", t0, result(t0, 40))); err != nil {
		t.Fatal(err)
	}
	hs := hybridFixture()
	u := okUpdate("claude", t0.Add(time.Minute), result(t0.Add(-time.Hour), 90))
	u.Hybrid = &hs
	if err := s.Commit(u); !errors.Is(err, ErrOlderObservation) {
		t.Fatal(err)
	}
	if h, ok := s.Hybrid("claude"); !ok || h.RemoteFailures != 2 {
		t.Fatal("hybrid dropped with older observation")
	}
}

func TestSetHybridWithPendingJournalDrains(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	if err := s.Commit(okUpdate("claude", t0, result(t0, 40))); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHybrid("claude", hybridFixture(), t0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "state", "claude.json")), `"hybrid"`) ||
		strings.Contains(readFile(t, filepath.Join(dir, "state", "claude.json")), "pending_") {
		t.Fatal("state shape")
	}
	if n := len(lines(t, filepath.Join(dir, "attempts", "claude.jsonl"))); n != 1 {
		t.Fatalf("attempts %d", n)
	}
}
