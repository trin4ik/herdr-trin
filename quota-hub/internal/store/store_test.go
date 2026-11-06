package store

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"quota-hub/internal/model"
)

var t0 = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

const secret = "sk-SECRET-RAW-TOKEN"

func defs() []model.Definition {
	return []model.Definition{
		{ID: "claude", Provider: "claude", Product: "code", Source: "claude_code", StaleAfter: 30 * time.Minute},
		{ID: "codex", Provider: "codex", Product: "cli", Source: "codex", StaleAfter: time.Hour},
	}
}

func f64(v float64) *float64 { return &v }

func result(obs time.Time, used float64) *model.Result {
	plan := "max"
	return &model.Result{
		ObservedAt: obs,
		Plan:       &plan,
		Quotas: []model.Quota{{ID: "5h", Metric: "percent", Unit: "percent", UsedPercent: f64(used),
			Window: model.Window{Kind: "rolling"}, Authority: "provider"}},
		Raw: json.RawMessage(`{"token":"` + secret + `","u":` + "1" + `}`),
	}
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, defs())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func okUpdate(id string, at time.Time, r *model.Result) model.Update {
	return model.Update{ID: id, AttemptAt: at, NextAttemptAt: at.Add(10 * time.Minute), Result: r}
}

func failUpdate(id string, at time.Time, code string) model.Update {
	return model.Update{ID: id, AttemptAt: at, NextAttemptAt: at.Add(time.Hour),
		Failure: &model.ErrorInfo{Code: code, HTTPStatus: 429}}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func lines(t *testing.T, p string) []string {
	t.Helper()
	s := strings.TrimSuffix(readFile(t, p), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestEmptySnapshotHasNoNullsOrZeros(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	snap := s.Snapshot(t0)
	b, _ := json.Marshal(snap)
	if strings.Contains(string(b), `"quotas":null`) || strings.Contains(string(b), `"metrics":null`) {
		t.Fatalf("nulls: %s", b)
	}
	sub := snap.Subscriptions[0]
	if sub.Status != "unknown" || !sub.Stale || sub.LastSuccessAt != nil || len(sub.Quotas) != 0 {
		t.Fatalf("%+v", sub)
	}
}

func TestCommitSuccessThenFailurePreservesLastGoodAndRaw(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	obs := t0.Add(-time.Minute)
	if err := s.Commit(okUpdate("claude", t0, result(obs, 40))); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(failUpdate("claude", t0.Add(time.Minute), "rate_limited")); err != nil {
		t.Fatal(err)
	}
	sub, _ := s.GetAt("claude", t0.Add(time.Minute))
	if sub.Status != "stale" || !sub.Stale || sub.Error == nil || sub.Error.Code != "rate_limited" {
		t.Fatalf("%+v", sub)
	}
	if sub.LastSuccessAt == nil || !sub.LastSuccessAt.Equal(obs) {
		t.Fatalf("last success must be source time: %v", sub.LastSuccessAt)
	}
	if len(sub.Quotas) != 1 || *sub.Quotas[0].UsedPercent != 40 || sub.Plan == nil || *sub.Plan != "max" {
		t.Fatalf("%+v", sub)
	}
	if !sub.NextAttemptAt.Equal(t0.Add(time.Minute + time.Hour)) {
		t.Fatalf("next %v", sub.NextAttemptAt)
	}
	state := readFile(t, filepath.Join(dir, "state", "claude.json"))
	if !strings.Contains(state, secret) {
		t.Fatal("raw last-good must be retained in private state")
	}
	if got := s.Failures("claude"); got != 1 {
		t.Fatalf("failures %d", got)
	}
}

func TestNoRawInPublicOrJournals(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	if err := s.Commit(okUpdate("claude", t0, result(t0, 10))); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(failUpdate("claude", t0.Add(time.Second), "boom")); err != nil {
		t.Fatal(err)
	}
	pub, _ := json.Marshal(s.Snapshot(t0))
	if strings.Contains(string(pub), secret) {
		t.Fatal("secret in snapshot")
	}
	for _, p := range []string{"history/claude.jsonl", "attempts/claude.jsonl"} {
		if strings.Contains(readFile(t, filepath.Join(dir, p)), secret) {
			t.Fatalf("secret in %s", p)
		}
	}
	if n := len(lines(t, filepath.Join(dir, "attempts", "claude.jsonl"))); n != 2 {
		t.Fatalf("attempt lines %d", n)
	}
	// pending journals must be drained on the happy path
	if strings.Contains(readFile(t, filepath.Join(dir, "state", "claude.json")), "pending_") {
		t.Fatal("pending not cleared")
	}
}

func TestPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "root")
	s := open(t, dir)
	defer s.Close()
	if err := s.Commit(okUpdate("claude", t0, result(t0, 1))); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "state", "history", "attempts"} {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%q %v %v", p, fi.Mode(), err)
		}
	}
	for _, p := range []string{"LOCK", "state/claude.json", "history/claude.jsonl", "attempts/claude.jsonl"} {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%q %v %v", p, fi.Mode(), err)
		}
	}
}

func TestSymlinkRootRejected(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	os.Mkdir(real, 0o700)
	link := filepath.Join(base, "link")
	os.Symlink(real, link)
	if _, err := Open(link, defs()); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatalf("got %v", err)
	}
}

func TestSymlinkStateRejected(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Close()
	target := filepath.Join(t.TempDir(), "x")
	os.WriteFile(target, []byte("{}"), 0o600)
	os.Symlink(target, filepath.Join(dir, "state", "claude.json"))
	if _, err := Open(dir, defs()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v", err)
	}
}

func TestLockPreventsSecondOpen(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := Open(dir, defs()); !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v", err)
	}
	s.Close()
	s2, err := Open(dir, defs())
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if err := s2.Commit(okUpdate("claude", t0, result(t0, 1))); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
	_ = syscall.Getpid()
}

func TestRestartHonorsCooldownAndLastGood(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Commit(okUpdate("claude", t0, result(t0, 25)))
	upd := failUpdate("claude", t0.Add(time.Minute), "rate_limited")
	upd.NextAttemptAt = t0.Add(5 * time.Hour)
	upd.Failure.RetryAt = &upd.NextAttemptAt
	if err := s.Commit(upd); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(t, dir)
	defer s.Close()
	sub, _ := s.GetAt("claude", t0.Add(2*time.Minute))
	if sub.NextAttemptAt == nil || !sub.NextAttemptAt.Equal(t0.Add(5*time.Hour)) {
		t.Fatalf("next %v", sub.NextAttemptAt)
	}
	if sub.Error == nil || sub.Error.RetryAt == nil || len(sub.Quotas) != 1 || s.Failures("claude") != 1 {
		t.Fatalf("%+v", sub)
	}
}

func TestSameObservationNoDuplicateHistory(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	for i := 0; i < 3; i++ {
		if err := s.Commit(okUpdate("claude", t0.Add(time.Duration(i)*30*time.Second), result(t0, 33))); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(lines(t, filepath.Join(dir, "history", "claude.jsonl"))); n != 1 {
		t.Fatalf("history %d", n)
	}
	if n := len(lines(t, filepath.Join(dir, "attempts", "claude.jsonl"))); n != 3 {
		t.Fatalf("attempts %d", n)
	}
	sub, _ := s.Get("claude")
	if !sub.LastSuccessAt.Equal(t0) {
		t.Fatalf("success time moved: %v", sub.LastSuccessAt)
	}
	if !sub.LastAttemptAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("attempt %v", sub.LastAttemptAt)
	}
	// new values at a new time do append
	s.Commit(okUpdate("claude", t0.Add(time.Hour), result(t0.Add(time.Minute), 34)))
	if n := len(lines(t, filepath.Join(dir, "history", "claude.jsonl"))); n != 2 {
		t.Fatalf("history %d", n)
	}
}

func TestOlderObservationDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	s.Commit(okUpdate("claude", t0, result(t0, 50)))
	err := s.Commit(okUpdate("claude", t0.Add(time.Minute), result(t0.Add(-time.Hour), 1)))
	if !errors.Is(err, ErrOlderObservation) {
		t.Fatalf("got %v", err)
	}
	sub, _ := s.GetAt("claude", t0.Add(time.Minute))
	if *sub.Quotas[0].UsedPercent != 50 || !sub.LastSuccessAt.Equal(t0) || sub.Error == nil || sub.Error.Code != CodeOlderObservation {
		t.Fatalf("%+v", sub)
	}
	if n := len(lines(t, filepath.Join(dir, "history", "claude.jsonl"))); n != 1 {
		t.Fatalf("history %d", n)
	}
	if n := len(lines(t, filepath.Join(dir, "attempts", "claude.jsonl"))); n != 2 {
		t.Fatalf("attempts %d", n)
	}
}

func TestStaleAfterWithoutFailure(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	s.Commit(okUpdate("claude", t0, result(t0, 5)))
	if sub, _ := s.GetAt("claude", t0.Add(29*time.Minute)); sub.Stale || sub.Status != "ok" {
		t.Fatalf("%+v", sub)
	}
	sub, _ := s.GetAt("claude", t0.Add(30*time.Minute))
	if !sub.Stale || sub.Status != "stale" || sub.StaleAfter == nil || len(sub.Quotas) != 1 {
		t.Fatalf("%+v", sub)
	}
}

func TestInvalidUpdates(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	nan := f64(math.NaN())
	cases := map[string]model.Update{
		"both":    {ID: "claude", AttemptAt: t0, NextAttemptAt: t0, Result: result(t0, 1), Failure: &model.ErrorInfo{Code: "x"}},
		"neither": {ID: "claude", AttemptAt: t0, NextAttemptAt: t0},
		"badid":   {ID: "../x", AttemptAt: t0, NextAttemptAt: t0, Failure: &model.ErrorInfo{Code: "x"}},
		"notime":  {ID: "claude", Failure: &model.ErrorInfo{Code: "x"}},
		"zeroobs": {ID: "claude", AttemptAt: t0, NextAttemptAt: t0, Result: &model.Result{}},
		"badraw":  {ID: "claude", AttemptAt: t0, NextAttemptAt: t0, Result: &model.Result{ObservedAt: t0, Raw: json.RawMessage(`{bad`)}},
		"nan":     {ID: "claude", AttemptAt: t0, NextAttemptAt: t0, Result: &model.Result{ObservedAt: t0, Quotas: []model.Quota{{UsedPercent: nan}}}},
	}
	for name, u := range cases {
		if err := s.Commit(u); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.Commit(failUpdate("nope", t0, "x")); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("got %v", err)
	}
	if sub, _ := s.Get("claude"); sub.LastAttemptAt != nil {
		t.Fatal("invalid update changed state")
	}
}

func TestFailureCodeSanitized(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	s.Commit(model.Update{ID: "claude", AttemptAt: t0, NextAttemptAt: t0.Add(time.Minute),
		Failure: &model.ErrorInfo{Code: "Get https://x?token=" + secret}})
	sub, _ := s.Get("claude")
	if sub.Error.Code != CodeCollectionError {
		t.Fatalf("%+v", sub.Error)
	}
}

func TestInvalidDefinitions(t *testing.T) {
	for _, id := range []string{"", "../x", "A", "-a", "a/b", strings.Repeat("a", 65), "a.b"} {
		if _, err := Open(t.TempDir(), []model.Definition{{ID: id}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", id, err)
		}
	}
	if _, err := Open(t.TempDir(), []model.Definition{{ID: "a"}, {ID: "a"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate accepted")
	}
}

func TestCorruptStateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Commit(okUpdate("claude", t0, result(t0, 1)))
	s.Close()
	p := filepath.Join(dir, "state", "claude.json")
	for _, bad := range []string{"{not json", `{"version":1,"id":"other"}`, `{"version":9,"id":"claude"}`, ""} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := Open(dir, defs()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	// failed Open must release the lock
	os.Remove(p)
	s, err := Open(dir, defs())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestCorruptCompleteJournalLineFailsClosed(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Commit(okUpdate("claude", t0, result(t0, 1)))
	s.Close()
	p := filepath.Join(dir, "attempts", "claude.jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("garbage\n")
	f.Close()
	if _, err := Open(dir, defs()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v", err)
	}
}

func TestIncompleteTrailingLineTruncatedOnly(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Commit(okUpdate("claude", t0, result(t0, 1)))
	s.Commit(failUpdate("claude", t0.Add(time.Minute), "x"))
	s.Close()
	p := filepath.Join(dir, "attempts", "claude.jsonl")
	before := readFile(t, p)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"id":"claude-3","sub`)
	f.Close()
	s = open(t, dir)
	defer s.Close()
	if got := readFile(t, p); got != before {
		t.Fatalf("not repaired exactly:\n%q\n%q", got, before)
	}
	if err := s.Commit(failUpdate("claude", t0.Add(2*time.Minute), "y")); err != nil {
		t.Fatal(err)
	}
	if n := len(lines(t, p)); n != 3 {
		t.Fatalf("lines %d", n)
	}
}

// Crash after the state commit but before any journal append.
func TestRecoveryBackfillsJournalAfterCrash(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Commit(okUpdate("claude", t0, result(t0, 1)))
	// simulate crash: journal paths blocked by directories => ErrJournal, state committed
	hp := filepath.Join(dir, "history", "claude.jsonl")
	ap := filepath.Join(dir, "attempts", "claude.jsonl")
	hb, ab := readFile(t, hp), readFile(t, ap)
	os.Remove(hp)
	os.Remove(ap)
	os.Mkdir(hp, 0o700)
	os.Mkdir(ap, 0o700)
	err := s.Commit(okUpdate("claude", t0.Add(time.Minute), result(t0.Add(time.Minute), 2)))
	if !errors.Is(err, ErrJournal) {
		t.Fatalf("got %v", err)
	}
	// visible state and cooldown are already committed; last-good preserved
	sub, _ := s.GetAt("claude", t0.Add(time.Minute))
	if *sub.Quotas[0].UsedPercent != 2 || sub.NextAttemptAt == nil {
		t.Fatalf("%+v", sub)
	}
	if err := s.Commit(failUpdate("claude", t0.Add(2*time.Minute), "net")); !errors.Is(err, ErrJournal) {
		t.Fatalf("got %v", err)
	}
	sub, _ = s.GetAt("claude", t0.Add(2*time.Minute))
	if *sub.Quotas[0].UsedPercent != 2 || sub.Error == nil {
		t.Fatalf("last-good lost: %+v", sub)
	}
	s.Close()
	os.Remove(hp)
	os.Remove(ap)
	os.WriteFile(hp, []byte(hb), 0o600)
	os.WriteFile(ap, []byte(ab), 0o600)
	s = open(t, dir)
	defer s.Close()
	if n := len(lines(t, hp)); n != 2 {
		t.Fatalf("history %d", n)
	}
	if n := len(lines(t, ap)); n != 3 {
		t.Fatalf("attempts %d", n)
	}
	sub, _ = s.Get("claude")
	if *sub.Quotas[0].UsedPercent != 2 || sub.Error == nil {
		t.Fatalf("%+v", sub)
	}
}

// Crash after the journal append but before the state clears pending: no duplicates.
func TestRecoveryNoDuplicateWhenLogWritten(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Commit(okUpdate("claude", t0, result(t0, 1)))
	s.Close()
	sp := filepath.Join(dir, "state", "claude.json")
	var st state
	json.Unmarshal([]byte(readFile(t, sp)), &st)
	// rebuild pending exactly as before the clear step
	hl := lines(t, filepath.Join(dir, "history", "claude.jsonl"))
	al := lines(t, filepath.Join(dir, "attempts", "claude.jsonl"))
	var h historyRecord
	var a attemptRecord
	json.Unmarshal([]byte(hl[0]), &h)
	json.Unmarshal([]byte(al[0]), &a)
	st.PendingHistory = []historyRecord{h}
	st.PendingAttempts = []attemptRecord{a}
	b, _ := json.Marshal(st)
	os.WriteFile(sp, b, 0o600)
	s = open(t, dir)
	defer s.Close()
	if len(lines(t, filepath.Join(dir, "history", "claude.jsonl"))) != 1 || len(lines(t, filepath.Join(dir, "attempts", "claude.jsonl"))) != 1 {
		t.Fatal("duplicated journal records")
	}
	if strings.Contains(readFile(t, sp), "pending_") {
		t.Fatal("pending not cleared")
	}
}

func TestDisabledAndRemovedIDsNotExposed(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Commit(okUpdate("codex", t0, result(t0, 7)))
	s.Close()
	s, err := Open(dir, defs()[:1])
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.Get("codex"); ok {
		t.Fatal("removed id exposed")
	}
	b, _ := json.Marshal(s.Snapshot(t0))
	if strings.Contains(string(b), "codex") {
		t.Fatalf("%s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "codex.json")); err != nil {
		t.Fatal("persisted data of removed id must stay")
	}
}

func TestConcurrentReadersAndDeepCopies(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	s.Commit(okUpdate("claude", t0, result(t0, 1)))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := s.Snapshot(t0)
				*snap.Subscriptions[0].Quotas[0].UsedPercent = 999 // must not leak into the store
				snap.Subscriptions[0].Quotas[0].ID = "mutated"
				if sub, ok := s.Get("claude"); ok && len(sub.Quotas) > 0 {
					sub.Quotas[0].Window.Kind = "mutated"
				}
			}
		}()
	}
	for i := 1; i <= 20; i++ {
		obs := t0.Add(time.Duration(i) * time.Second)
		if err := s.Commit(okUpdate("claude", obs, result(obs, float64(i)))); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	sub, _ := s.Get("claude")
	if sub.Quotas[0].ID != "5h" || *sub.Quotas[0].UsedPercent != 20 || sub.Quotas[0].Window.Kind != "rolling" {
		t.Fatalf("%+v", sub.Quotas[0])
	}
}
