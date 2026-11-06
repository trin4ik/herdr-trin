package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"quota-hub/internal/model"
)

const (
	rpA      = "fp-key-generation-A"
	rpB      = "fp-key-generation-B"
	rpEmailA = "alice@example.test"
	rpEmailB = "bob@example.test"
)

func rpDefs(publish bool) []model.Definition {
	return []model.Definition{
		{ID: "ollama", Provider: "ollama", Product: "cloud", Source: "opencode_key", StaleAfter: time.Hour, PublishAccountEmail: publish},
		{ID: "ollama2", Provider: "ollama", Product: "cloud", Source: "opencode_key", StaleAfter: time.Hour, PublishAccountEmail: publish},
		{ID: "kimi", Provider: "kimi", Product: "code", Source: "opencode_key", StaleAfter: time.Hour, PublishAccountEmail: publish},
	}
}

func rpOpen(t *testing.T, dir string, publish bool) *Store {
	t.Helper()
	s, err := Open(dir, rpDefs(publish))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func rpUsage(id string, at time.Time, ident string) model.Update {
	r := result(at, 10)
	r.Plan = nil // Ollama/Kimi usage endpoints do not report a plan
	r.ProfileIdentity = ident
	return okUpdate(id, at, r)
}

func rpUsageFail(id string, at time.Time, ident string, checked bool) model.Update {
	return model.Update{ID: id, AttemptAt: at, NextAttemptAt: at.Add(time.Hour),
		Failure: &model.ErrorInfo{Code: "http_500", HTTPStatus: 500, ProfileIdentity: ident, ProfileIdentityChecked: checked}}
}

func rpOK(ident string, at time.Time, email, plan string) model.ProfileUpdate {
	return model.ProfileUpdate{ExpectedIdentity: ident, CurrentIdentity: ident, IdentityChecked: true,
		AttemptAt: at, NextAttemptAt: at.Add(6 * time.Hour), Profile: prof(ident, email, plan)}
}

func rpFail(ident string, at time.Time, f model.ErrorInfo) model.ProfileUpdate {
	return model.ProfileUpdate{ExpectedIdentity: ident, CurrentIdentity: ident, IdentityChecked: true,
		AttemptAt: at, NextAttemptAt: at.Add(5 * time.Minute), Failure: &f}
}

func rpMust(t *testing.T, s *Store, id string, u model.ProfileUpdate) {
	t.Helper()
	if err := s.CommitProfile(id, u); err != nil {
		t.Fatal(err)
	}
}

func rpSched(t *testing.T, s *Store, id string) model.ProfileState {
	t.Helper()
	p, ok := s.RemoteProfileSchedule(id)
	if !ok {
		t.Fatalf("no remote profile state for %s", id)
	}
	return p
}

func rpNoMeta(t *testing.T, s *Store, id string) {
	t.Helper()
	sub, _ := s.GetAt(id, t0.Add(time.Minute))
	if sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("metadata leaked: email=%v plan=%v", sub.AccountEmail, sub.Plan)
	}
}

func rpMeta(t *testing.T, s *Store, id, email, plan string) {
	t.Helper()
	sub, _ := s.GetAt(id, t0.Add(time.Minute))
	if !eqStr(sub.AccountEmail, email) || !eqStr(sub.Plan, plan) {
		t.Fatalf("metadata = %v / %v, want %s / %s", sub.AccountEmail, sub.Plan, email, plan)
	}
}

func rpJournalLines(t *testing.T, dir, kind, id string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, kind, id+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestRemoteProfileSelectedByUsageAndCommitIsMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	s := rpOpen(t, dir, true)

	if _, ok := s.RemoteProfile("ollama"); ok {
		t.Fatal("profile state before any usage")
	}
	if err := s.CommitProfile("ollama", rpOK(rpA, t0, rpEmailA, "Pro")); !errors.Is(err, ErrOlderObservation) {
		t.Fatalf("profile before usage identity: %v", err)
	}

	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	st, ok := s.RemoteProfile("ollama")
	if !ok || st.Identity != rpA || st.NextAttemptAt != nil || st.LastAttemptAt != nil || st.Failures != 0 || st.Error != nil {
		t.Fatalf("selected state: %+v %v", st, ok)
	}
	rpNoMeta(t, s, "ollama")

	before := getSub(t, s, "ollama")
	failuresBefore := s.Failures("ollama")
	attemptsBefore, historyBefore := rpJournalLines(t, dir, "attempts", "ollama"), rpJournalLines(t, dir, "history", "ollama")

	at := t0.Add(time.Minute)
	rpMust(t, s, "ollama", rpOK(rpA, at, rpEmailA, "Pro"))
	rpMeta(t, s, "ollama", rpEmailA, "Pro")

	after := getSub(t, s, "ollama")
	if !after.LastAttemptAt.Equal(*before.LastAttemptAt) || !after.NextAttemptAt.Equal(*before.NextAttemptAt) ||
		!after.LastSuccessAt.Equal(*before.LastSuccessAt) || after.Error != nil || after.Status != before.Status ||
		len(after.Quotas) != len(before.Quotas) || s.Failures("ollama") != failuresBefore {
		t.Fatalf("usage state changed by profile commit:\nbefore %+v\nafter  %+v", before, after)
	}
	if rpJournalLines(t, dir, "attempts", "ollama") != attemptsBefore || rpJournalLines(t, dir, "history", "ollama") != historyBefore {
		t.Fatal("profile commit touched usage journals")
	}
	st = rpSched(t, s, "ollama")
	if st.LastAttemptAt == nil || !st.LastAttemptAt.Equal(at) || st.NextAttemptAt == nil || !st.NextAttemptAt.Equal(at.Add(6*time.Hour)) {
		t.Fatalf("schedule %+v", st)
	}
	if v := readFile(t, filepath.Join(dir, "state", "ollama.json")); !strings.Contains(v, `"version":3`) || !strings.Contains(v, "remote_profile") {
		t.Fatalf("state is not v3: %s", v)
	}
}

func TestCommitProfileRejectsStaleAndInvalidWithoutMutation(t *testing.T) {
	s := rpOpen(t, t.TempDir(), true)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	rpMust(t, s, "ollama", rpOK(rpA, t0, rpEmailA, "Pro"))
	want := rpSched(t, s, "ollama")

	good := rpOK(rpA, t0.Add(time.Hour), rpEmailB, "Max")
	mut := func(f func(*model.ProfileUpdate)) model.ProfileUpdate { u := good; f(&u); return u }
	cases := []struct {
		name string
		u    model.ProfileUpdate
		err  error
	}{
		{"empty expected", mut(func(u *model.ProfileUpdate) { u.ExpectedIdentity = "" }), ErrOlderObservation},
		{"stale expected", mut(func(u *model.ProfileUpdate) {
			u.ExpectedIdentity = rpB
			u.CurrentIdentity = rpB
			u.Profile = prof(rpB, rpEmailB, "Max")
		}), ErrOlderObservation},
		{"zero attempt", mut(func(u *model.ProfileUpdate) { u.AttemptAt = time.Time{} }), ErrInvalid},
		{"zero next", mut(func(u *model.ProfileUpdate) { u.NextAttemptAt = time.Time{} }), ErrInvalid},
		{"neither", mut(func(u *model.ProfileUpdate) { u.Profile = nil }), ErrInvalid},
		{"both", mut(func(u *model.ProfileUpdate) { u.Failure = &model.ErrorInfo{Code: "x"} }), ErrInvalid},
		{"profile for other identity", mut(func(u *model.ProfileUpdate) { u.Profile = prof(rpB, rpEmailB, "Max") }), ErrInvalid},
		{"unchecked success", mut(func(u *model.ProfileUpdate) { u.IdentityChecked = false }), ErrInvalid},
		{"current differs on success", mut(func(u *model.ProfileUpdate) { u.CurrentIdentity = rpB }), ErrInvalid},
		{"unverifiable current on success", mut(func(u *model.ProfileUpdate) { u.CurrentIdentity = "" }), ErrInvalid},
	}
	for _, c := range cases {
		if err := s.CommitProfile("ollama", c.u); !errors.Is(err, c.err) {
			t.Fatalf("%s: %v, want %v", c.name, err, c.err)
		}
		rpMeta(t, s, "ollama", rpEmailA, "Pro")
		got := rpSched(t, s, "ollama")
		if got.Identity != want.Identity || !got.NextAttemptAt.Equal(*want.NextAttemptAt) || got.Failures != want.Failures || got.Error != nil {
			t.Fatalf("%s mutated state: %+v", c.name, got)
		}
	}
	if err := s.CommitProfile("nope", good); !errors.Is(err, ErrUnknownID) {
		t.Fatal(err)
	}
	s.Close()
	if err := s.CommitProfile("ollama", good); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestNullProfileIsAuthoritativeAndErrorsPreserveSameAccountMetadata(t *testing.T) {
	s := rpOpen(t, t.TempDir(), true)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	rpMust(t, s, "ollama", rpOK(rpA, t0, rpEmailA, "Pro"))

	// failures keep same-account metadata, count independently and never touch usage
	for i, f := range []model.ErrorInfo{
		{Code: "timeout"},
		{Code: "http_500", HTTPStatus: 500},
	} {
		rpMust(t, s, "ollama", rpFail(rpA, t0.Add(time.Duration(i+1)*time.Hour), f))
		rpMeta(t, s, "ollama", rpEmailA, "Pro")
		st := rpSched(t, s, "ollama")
		if st.Failures != i+1 || st.Error == nil || st.Error.Code != f.Code {
			t.Fatalf("failure %d: %+v", i, st)
		}
		if sub := getSub(t, s, "ollama"); sub.Error != nil || s.Failures("ollama") != 0 || sub.Status != "ok" {
			t.Fatalf("root usage state polluted: %+v", sub)
		}
	}

	// a successful profile with nothing in it clears both fields
	rpMust(t, s, "ollama", rpOK(rpA, t0.Add(3*time.Hour), "", ""))
	rpNoMeta(t, s, "ollama")
	st := rpSched(t, s, "ollama")
	if st.Failures != 0 || st.Error != nil || st.Identity != rpA {
		t.Fatalf("success must reset profile failure state: %+v", st)
	}

	// invalid values from the provider are dropped, never stored verbatim
	bad := rpOK(rpA, t0.Add(4*time.Hour), "", "")
	bad.Profile.AccountEmail, bad.Profile.Plan = ptr("not an email"), ptr("Pro\nTeam")
	rpMust(t, s, "ollama", bad)
	rpNoMeta(t, s, "ollama")
}

func TestKeySwitchClearsMetadataAndDropsSuccessTTL(t *testing.T) {
	s := rpOpen(t, t.TempDir(), true)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	rpMust(t, s, "ollama", rpOK(rpA, t0, rpEmailA, "Pro"))

	mustCommit(t, s, rpUsage("ollama", t0.Add(time.Minute), rpB))
	rpNoMeta(t, s, "ollama")
	st, ok := s.RemoteProfile("ollama")
	if !ok || st.Identity != rpB || st.NextAttemptAt != nil || st.LastAttemptAt != nil || st.Failures != 0 {
		t.Fatalf("new generation must be fetchable immediately: %+v %v", st, ok)
	}

	// a response that was in flight for the old key is rejected
	if err := s.CommitProfile("ollama", rpOK(rpA, t0.Add(2*time.Minute), rpEmailA, "Pro")); !errors.Is(err, ErrOlderObservation) {
		t.Fatalf("stale response accepted: %v", err)
	}
	rpNoMeta(t, s, "ollama")

	rpMust(t, s, "ollama", rpOK(rpB, t0.Add(3*time.Minute), rpEmailB, "Max"))
	rpMeta(t, s, "ollama", rpEmailB, "Max")

	// returning to the first key never resurrects the first key's metadata
	mustCommit(t, s, rpUsage("ollama", t0.Add(4*time.Minute), rpA))
	rpNoMeta(t, s, "ollama")
	if st, _ := s.RemoteProfile("ollama"); st.Identity != rpA || st.NextAttemptAt != nil {
		t.Fatalf("%+v", st)
	}
}

func TestKeySwitchKeepsFailureCooldown(t *testing.T) {
	s := rpOpen(t, t.TempDir(), true)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	rpMust(t, s, "ollama", rpOK(rpA, t0, rpEmailA, "Pro"))
	retry := t0.Add(9 * time.Hour)
	f := rpFail(rpA, t0.Add(time.Hour), model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retry})
	f.NextAttemptAt = retry
	rpMust(t, s, "ollama", f)

	mustCommit(t, s, rpUsage("ollama", t0.Add(2*time.Hour), rpB))
	rpNoMeta(t, s, "ollama")
	st := rpSched(t, s, "ollama")
	if st.Identity != rpB || st.NextAttemptAt == nil || !st.NextAttemptAt.Equal(retry) || st.Error == nil ||
		st.Error.HTTPStatus != 429 || st.Error.RetryAt == nil || !st.Error.RetryAt.Equal(retry) || st.Failures != 1 {
		t.Fatalf("cooldown must survive a key switch: %+v", st)
	}
}

func TestIdentityUnknownHidesOnlyAndSameIdentityReveals(t *testing.T) {
	dir := t.TempDir()
	s := rpOpen(t, dir, true)
	mustCommit(t, s, rpUsage("kimi", t0, rpA))
	rpMust(t, s, "kimi", rpOK(rpA, t0, rpEmailA, "Pro"))
	before := rpSched(t, s, "kimi")

	// unverifiable usage failure: hide, keep cooldown, last-good and root error semantics
	mustCommit(t, s, rpUsageFail("kimi", t0.Add(time.Minute), "", true))
	rpNoMeta(t, s, "kimi")
	if _, ok := s.RemoteProfile("kimi"); ok {
		t.Fatal("hidden state must not be returned for display")
	}
	st := rpSched(t, s, "kimi")
	if st.Identity != rpA || !st.NextAttemptAt.Equal(*before.NextAttemptAt) {
		t.Fatalf("schedule lost: %+v", st)
	}
	sub := getSub(t, s, "kimi")
	if sub.Error == nil || sub.Error.Code != "http_500" || sub.LastSuccessAt == nil || len(sub.Quotas) == 0 {
		t.Fatalf("root error/last-good: %+v", sub)
	}

	// an unchecked failure says nothing about the credential
	mustCommit(t, s, rpUsageFail("kimi", t0.Add(2*time.Minute), "ignored", false))
	rpNoMeta(t, s, "kimi")

	// the same credential generation confirms and reveals the cached metadata, unchanged
	mustCommit(t, s, rpUsage("kimi", t0.Add(3*time.Minute), rpA))
	rpMeta(t, s, "kimi", rpEmailA, "Pro")
	if st := rpSched(t, s, "kimi"); !st.NextAttemptAt.Equal(*before.NextAttemptAt) {
		t.Fatalf("TTL changed: %+v", st)
	}

	// a failed usage request that still proves the key also reveals it
	mustCommit(t, s, rpUsageFail("kimi", t0.Add(4*time.Minute), "", true))
	mustCommit(t, s, rpUsageFail("kimi", t0.Add(5*time.Minute), rpA, true))
	rpMeta(t, s, "kimi", rpEmailA, "Pro")

	// a failed usage request that proves a different key clears it
	mustCommit(t, s, rpUsageFail("kimi", t0.Add(6*time.Minute), rpB, true))
	rpNoMeta(t, s, "kimi")
	if st := rpSched(t, s, "kimi"); st.Identity != rpB || st.NextAttemptAt != nil {
		t.Fatalf("%+v", st)
	}
}

func TestProfileFailureWithDifferentCurrentIdentityNeverLeaksOldMetadata(t *testing.T) {
	s := rpOpen(t, t.TempDir(), true)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	rpMust(t, s, "ollama", rpOK(rpA, t0, rpEmailA, "Pro"))

	u := rpFail(rpA, t0.Add(time.Minute), model.ErrorInfo{Code: "profile_identity_changed"})
	u.CurrentIdentity = rpB
	rpMust(t, s, "ollama", u)
	rpNoMeta(t, s, "ollama")
	if st := rpSched(t, s, "ollama"); st.Identity != rpB {
		t.Fatalf("%+v", st)
	}

	// unverifiable after the response: hide, keep recording the failure for the same key
	mustCommit(t, s, rpUsage("ollama", t0.Add(2*time.Minute), rpA))
	rpMust(t, s, "ollama", rpOK(rpA, t0.Add(2*time.Minute), rpEmailA, "Pro"))
	u = rpFail(rpA, t0.Add(3*time.Minute), model.ErrorInfo{Code: "credentials_unavailable"})
	u.CurrentIdentity = ""
	rpMust(t, s, "ollama", u)
	rpNoMeta(t, s, "ollama")
	if st := rpSched(t, s, "ollama"); st.Identity != rpA || st.Error == nil {
		t.Fatalf("%+v", st)
	}
}

func TestHideRemoteProfileAndRestartKeepCooldown(t *testing.T) {
	dir := t.TempDir()
	s := rpOpen(t, dir, true)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	rpMust(t, s, "ollama", rpOK(rpA, t0, rpEmailA, "Pro"))
	// a server date far beyond the 6h cache must be stored as is
	retry := t0.Add(30 * time.Hour)
	f := rpFail(rpA, t0.Add(7*time.Hour), model.ErrorInfo{Code: "rate_limited", HTTPStatus: 429, RetryAt: &retry})
	f.NextAttemptAt = retry
	rpMust(t, s, "ollama", f)
	s.Close()

	s = rpOpen(t, dir, true)
	st := rpSched(t, s, "ollama")
	if st.Identity != rpA || st.Failures != 1 || !st.NextAttemptAt.Equal(retry) || st.Error == nil || !st.Error.RetryAt.Equal(retry) {
		t.Fatalf("restored: %+v", st)
	}
	if err := s.HideRemoteProfile("ollama"); err != nil {
		t.Fatal(err)
	}
	if err := s.HideRemoteProfile("ollama"); err != nil {
		t.Fatal(err)
	}
	rpNoMeta(t, s, "ollama")
	if _, ok := s.RemoteProfile("ollama"); ok {
		t.Fatal("hidden")
	}
	if st := rpSched(t, s, "ollama"); !st.NextAttemptAt.Equal(retry) {
		t.Fatalf("hide changed schedule: %+v", st)
	}
	if err := s.HideRemoteProfile("kimi"); err != nil {
		t.Fatalf("no profile state is a no-op: %v", err)
	}
	if err := s.HideRemoteProfile("zzz"); !errors.Is(err, ErrUnknownID) {
		t.Fatal(err)
	}

	// hiding persists; same key reveals, metadata was never deleted
	s.Close()
	s = rpOpen(t, dir, true)
	rpNoMeta(t, s, "ollama")
	mustCommit(t, s, rpUsage("ollama", t0.Add(31*time.Hour), rpA))
	rpMeta(t, s, "ollama", rpEmailA, "Pro")
	if st := rpSched(t, s, "ollama"); !st.NextAttemptAt.Equal(retry) || st.Failures != 1 {
		t.Fatalf("%+v", st)
	}
	s.Close()
	if err := s.HideRemoteProfile("ollama"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestTwoAccountsOfOneProviderAreIndependent(t *testing.T) {
	s := rpOpen(t, t.TempDir(), true)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	mustCommit(t, s, rpUsage("ollama2", t0, rpB))
	rpMust(t, s, "ollama", rpOK(rpA, t0, rpEmailA, "Pro"))
	rpMust(t, s, "ollama2", rpOK(rpB, t0, rpEmailB, "Max"))

	// the other account's key does not belong here
	if err := s.CommitProfile("ollama", rpOK(rpB, t0, rpEmailB, "Max")); !errors.Is(err, ErrOlderObservation) {
		t.Fatalf("cross-account commit accepted: %v", err)
	}
	rpMeta(t, s, "ollama", rpEmailA, "Pro")
	rpMeta(t, s, "ollama2", rpEmailB, "Max")

	rpMust(t, s, "ollama2", rpFail(rpB, t0.Add(time.Hour), model.ErrorInfo{Code: "http_500", HTTPStatus: 500}))
	mustCommit(t, s, rpUsage("ollama", t0.Add(time.Hour), "fp-key-generation-C"))
	rpNoMeta(t, s, "ollama")
	rpMeta(t, s, "ollama2", rpEmailB, "Max")
	if st := rpSched(t, s, "ollama"); st.Error != nil || st.Failures != 0 {
		t.Fatalf("failure leaked across accounts: %+v", st)
	}
	if st := rpSched(t, s, "ollama2"); st.Error == nil || st.Failures != 1 || st.Identity != rpB {
		t.Fatalf("%+v", st)
	}

	b, _ := json.Marshal(s.Snapshot(t0.Add(time.Minute)))
	if strings.Contains(string(b), rpEmailA) || !strings.Contains(string(b), rpEmailB) {
		t.Fatalf("snapshot: %s", b)
	}
}

func TestInvalidatedSourceIdentityPermanentlyBlocksRemoteProfile(t *testing.T) {
	s := rpOpen(t, t.TempDir(), true)
	r := result(t0, 10)
	r.Identity, r.ProfileIdentity = "quota-account-1", rpA
	mustCommit(t, s, okUpdate("kimi", t0, r))
	rpMust(t, s, "kimi", rpOK(rpA, t0, rpEmailA, "Pro"))

	r2 := result(t0.Add(time.Minute), 11)
	r2.Identity, r2.ProfileIdentity = "quota-account-2", rpA
	if err := s.Commit(okUpdate("kimi", t0.Add(time.Minute), r2)); !errors.Is(err, ErrIdentityChanged) {
		t.Fatal(err)
	}
	rpNoMeta(t, s, "kimi")
	if _, ok := s.RemoteProfile("kimi"); ok {
		t.Fatal("invalidated state exposed")
	}
	if err := s.CommitProfile("kimi", rpOK(rpA, t0.Add(2*time.Minute), rpEmailB, "Max")); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("late response after invalidation: %v", err)
	}
	// even the original key cannot resurrect the metadata for this ID
	r3 := result(t0.Add(3*time.Minute), 12)
	r3.Plan = nil
	r3.Identity, r3.ProfileIdentity = "quota-account-1", rpA
	s.Commit(okUpdate("kimi", t0.Add(3*time.Minute), r3))
	rpNoMeta(t, s, "kimi")
	if _, ok := s.RemoteProfile("kimi"); ok {
		t.Fatal("invalidated state revived")
	}
}

func TestRemoteProfileNeverLeaksIntoPublicOrJournals(t *testing.T) {
	dir := t.TempDir()
	s := rpOpen(t, dir, false)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	rpMust(t, s, "ollama", rpOK(rpA, t0, rpEmailA, "Pro"))
	rpMust(t, s, "ollama", rpFail(rpA, t0.Add(time.Hour), model.ErrorInfo{Code: "profile_secret_code", HTTPStatus: 503, ProfileIdentity: "fp-embedded", ProfileIdentityChecked: true}))
	mustCommit(t, s, rpUsageFail("ollama", t0.Add(2*time.Hour), rpA, true))
	mustCommit(t, s, rpUsage("ollama2", t0, rpB)) // never fetched: no metadata at all

	snap := s.Snapshot(t0.Add(2 * time.Hour))
	b, _ := json.Marshal(snap)
	for _, bad := range []string{"fp-", rpEmailA, "example.test", "profile_secret_code", "remote_profile", "profile_identity"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("public snapshot leaks %q: %s", bad, b)
		}
	}
	if sub := snap.Subscriptions[0]; sub.Error == nil || sub.Error.Code != "http_500" || sub.AccountEmail != nil || !eqStr(sub.Plan, "Pro") {
		t.Fatalf("root error must be the usage error only: %+v", sub)
	}
	for _, kind := range []string{"attempts", "history"} {
		for _, id := range []string{"ollama", "ollama2"} {
			if p := filepath.Join(dir, kind, id+".jsonl"); fileExists(p) {
				if v := readFile(t, p); strings.Contains(v, "fp-") || strings.Contains(v, "example.test") || strings.Contains(v, "profile_secret") {
					t.Fatalf("%s/%s leaks: %s", kind, id, v)
				}
			}
		}
	}
	// the private state keeps the key fingerprint but never an error's identity fields
	v := readFile(t, filepath.Join(dir, "state", "ollama.json"))
	if strings.Contains(v, "fp-embedded") || strings.Contains(v, "profile_identity_checked") || strings.Contains(v, "ProfileIdentity") {
		t.Fatalf("error identity persisted: %s", v)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "state", "ollama2.json")), "example.test") {
		t.Fatal("other account state holds metadata")
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestProfileFailureSanitized(t *testing.T) {
	s := rpOpen(t, t.TempDir(), true)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	local := time.FixedZone("x", 3*3600)
	retry := time.Date(2026, 11, 5, 12, 0, 0, 0, local)
	for _, f := range []model.ErrorInfo{
		{Code: "bad code\nwith secret " + rpA, HTTPStatus: 999, RetryAt: &retry},
	} {
		rpMust(t, s, "ollama", rpFail(rpA, t0, f))
	}
	st := rpSched(t, s, "ollama")
	if st.Error.Code != CodeCollectionError || st.Error.HTTPStatus != 0 || st.Error.RetryAt == nil || st.Error.RetryAt.Location() != time.UTC || !st.Error.RetryAt.Equal(retry) {
		t.Fatalf("%+v", st.Error)
	}
	// returned copies are deep
	st.Error.Code = "mutated"
	*st.NextAttemptAt = time.Time{}
	if again := rpSched(t, s, "ollama"); again.Error.Code != CodeCollectionError || again.NextAttemptAt.IsZero() {
		t.Fatal("RemoteProfileSchedule leaks internal pointers")
	}
}

func TestRemoteProfileConcurrentUsageAndProfileCommits(t *testing.T) {
	s := rpOpen(t, t.TempDir(), true)
	mustCommit(t, s, rpUsage("ollama", t0, rpA))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.Snapshot(t0)
				s.RemoteProfile("ollama")
				s.RemoteProfileSchedule("ollama")
			}
		}()
	}
	for i := 1; i <= 30; i++ {
		at := t0.Add(time.Duration(i) * time.Second)
		ident := rpA
		if i%2 == 0 {
			ident = rpB
		}
		mustCommit(t, s, rpUsage("ollama", at, ident))
		// racing responses for either key: only the current one may apply
		for _, k := range []string{rpA, rpB} {
			email := rpEmailA
			if k == rpB {
				email = rpEmailB
			}
			err := s.CommitProfile("ollama", rpOK(k, at, email, "Pro"))
			if err != nil && !errors.Is(err, ErrOlderObservation) {
				t.Fatal(err)
			}
			if (err == nil) != (k == ident) {
				t.Fatalf("i=%d key %s applied=%v", i, k, err == nil)
			}
		}
	}
	close(stop)
	wg.Wait()
	rpMeta(t, s, "ollama", rpEmailB, "Pro")
}
