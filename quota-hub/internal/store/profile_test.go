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

const (
	pEmail  = "alice@example.test"
	pEmail2 = "bob@example.test"
)

func ptr(s string) *string { return &s }

func profDefs(publish bool) []model.Definition {
	return []model.Definition{
		{ID: "claude", Provider: "claude", Product: "code", Source: "claude_code", StaleAfter: 30 * time.Minute, PublishAccountEmail: publish},
	}
}

func profStore(t *testing.T, dir string, publish bool) *Store {
	t.Helper()
	s, err := Open(dir, profDefs(publish))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// profResult is a usage result with a private source identity and profile.
func profResult(obs time.Time, used float64, ident string, p *model.Profile) *model.Result {
	r := result(obs, used)
	r.Identity = ident
	r.Profile = p
	return r
}

func prof(ident, email, plan string) *model.Profile {
	p := &model.Profile{Identity: ident}
	if email != "" {
		p.AccountEmail = ptr(email)
	}
	if plan != "" {
		p.Plan = ptr(plan)
	}
	return p
}

func mustCommit(t *testing.T, s *Store, u model.Update) {
	t.Helper()
	if err := s.Commit(u); err != nil {
		t.Fatal(err)
	}
}

func getSub(t *testing.T, s *Store, id string) model.Subscription {
	t.Helper()
	sub, ok := s.GetAt(id, t0.Add(time.Minute))
	if !ok {
		t.Fatalf("missing %s", id)
	}
	return sub
}

func eqStr(p *string, want string) bool { return p != nil && *p == want }

func TestAccountEmailIsOptInPerDefinition(t *testing.T) {
	dir := t.TempDir()
	s := profStore(t, dir, false)
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	sub := getSub(t, s, "claude")
	if sub.AccountEmail != nil {
		t.Fatalf("email must be masked by default: %v", *sub.AccountEmail)
	}
	if !eqStr(sub.Plan, "Pro") {
		t.Fatalf("plan is not email and stays visible: %v", sub.Plan)
	}
	b, _ := json.Marshal(s.Snapshot(t0.Add(time.Minute)))
	if strings.Contains(string(b), "example.test") || !strings.Contains(string(b), `"account_email":null`) {
		t.Fatalf("masked snapshot: %s", b)
	}
	s.Close()

	// The flag is operator config, not part of the persisted definition: the
	// same state flips to published on restart and back again.
	s = profStore(t, dir, true)
	sub = getSub(t, s, "claude")
	if !eqStr(sub.AccountEmail, pEmail) || !eqStr(sub.Plan, "Pro") {
		t.Fatalf("opt-in: %+v", sub)
	}
	b, _ = json.Marshal(s.Snapshot(t0.Add(time.Minute)))
	if !strings.Contains(string(b), `"account_email":"`+pEmail+`"`) {
		t.Fatalf("published snapshot: %s", b)
	}
	s.Close()

	s = profStore(t, dir, false)
	defer s.Close()
	if getSub(t, s, "claude").AccountEmail != nil {
		t.Fatal("email leaked after flag turned off")
	}
}

func TestProfileSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := profStore(t, dir, true)
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	s.Close()

	s = profStore(t, dir, true)
	defer s.Close()
	sub := getSub(t, s, "claude")
	if !eqStr(sub.AccountEmail, pEmail) || !eqStr(sub.Plan, "Pro") {
		t.Fatalf("restored: %+v", sub)
	}
	// Restored identity still guards: a different account is blocked.
	if err := s.VerifyProfileIdentity("claude", "B", true); err != nil {
		t.Fatal(err)
	}
	sub = getSub(t, s, "claude")
	if sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("not hidden after restart+switch: %+v", sub)
	}
}

func TestNilProfilePreservedOnTemporaryError(t *testing.T) {
	dir := t.TempDir()
	s := profStore(t, dir, true)
	defer s.Close()
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	for i, code := range []string{"http_429", "network_error", "http_500", "auth_expired"} {
		mustCommit(t, s, failUpdate("claude", t0.Add(time.Duration(i+1)*time.Minute), code))
		sub := getSub(t, s, "claude")
		if sub.Error == nil || sub.Status != "stale" {
			t.Fatalf("%s: %+v", code, sub)
		}
		if !eqStr(sub.AccountEmail, pEmail) || !eqStr(sub.Plan, "Pro") {
			t.Fatalf("%s lost profile: %+v", code, sub)
		}
	}
	// A success without profile (nil = not checked) also preserves it, even
	// with a different result Plan; usage still advances.
	r := profResult(t0.Add(time.Hour), 55, "A", nil)
	r.Plan = ptr("max")
	mustCommit(t, s, okUpdate("claude", t0.Add(time.Hour), r))
	sub := getSub(t, s, "claude")
	if !eqStr(sub.AccountEmail, pEmail) || !eqStr(sub.Plan, "Pro") {
		t.Fatalf("nil profile did not preserve: %+v", sub)
	}
	if sub.Quotas[0].UsedPercent == nil || *sub.Quotas[0].UsedPercent != 55 {
		t.Fatalf("usage not updated: %+v", sub.Quotas)
	}
}

func TestExplicitProfileWithNullFieldsClears(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	defer s.Close()
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))

	// Authoritative profile with null email/plan: clears both, including the
	// result.Plan fallback ("max").
	t1 := t0.Add(time.Minute)
	mustCommit(t, s, okUpdate("claude", t1, profResult(t1, 20, "A", &model.Profile{Identity: "A"})))
	sub := getSub(t, s, "claude")
	if sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("not cleared: email=%v plan=%v", sub.AccountEmail, sub.Plan)
	}
	if sub.Status != "ok" || sub.Quotas[0].UsedPercent == nil || *sub.Quotas[0].UsedPercent != 20 {
		t.Fatalf("usage affected: %+v", sub)
	}

	// Only email is null: plan from profile stays.
	t2 := t1.Add(time.Minute)
	mustCommit(t, s, okUpdate("claude", t2, profResult(t2, 30, "A", prof("A", "", "Team"))))
	sub = getSub(t, s, "claude")
	if sub.AccountEmail != nil || !eqStr(sub.Plan, "Team") {
		t.Fatalf("partial: %+v", sub)
	}
}

func TestPlanBehavior(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	defer s.Close()
	// No profile at all: Plan comes from the result.
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", nil)))
	if sub := getSub(t, s, "claude"); !eqStr(sub.Plan, "max") || sub.AccountEmail != nil {
		t.Fatalf("result plan fallback: %+v", sub)
	}
	// Profile plan overrides the result plan.
	t1 := t0.Add(time.Minute)
	mustCommit(t, s, okUpdate("claude", t1, profResult(t1, 11, "A", prof("A", pEmail, "Pro"))))
	if sub := getSub(t, s, "claude"); !eqStr(sub.Plan, "Pro") {
		t.Fatalf("profile plan: %+v", sub.Plan)
	}
	// Invalidation drops the plan entirely (no fallback to result plan).
	if err := s.VerifyProfileIdentity("claude", "B", true); err != nil {
		t.Fatal(err)
	}
	if sub := getSub(t, s, "claude"); sub.Plan != nil || sub.AccountEmail != nil {
		t.Fatalf("invalidated: %+v", sub)
	}
}

func TestOlderObservationDoesNotApplyProfile(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	defer s.Close()
	t1 := t0.Add(time.Hour)
	mustCommit(t, s, okUpdate("claude", t1, profResult(t1, 10, "A", prof("A", pEmail, "Pro"))))
	err := s.Commit(okUpdate("claude", t1.Add(time.Minute), profResult(t0, 5, "A", prof("A", pEmail2, "Free"))))
	if !errors.Is(err, ErrOlderObservation) {
		t.Fatalf("%v", err)
	}
	sub := getSub(t, s, "claude")
	if !eqStr(sub.AccountEmail, pEmail) || !eqStr(sub.Plan, "Pro") {
		t.Fatalf("older result changed profile: %+v", sub)
	}
}

func TestVerifyProfileIdentityBlocksPermanentlyAndLateOldResultCannotRestore(t *testing.T) {
	dir := t.TempDir()
	s := profStore(t, dir, true)
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	t1 := t0.Add(time.Minute)
	mustCommit(t, s, failUpdate("claude", t1, "http_429"))
	before := getSub(t, s, "claude")
	failures := s.Failures("claude")
	hist := lines(t, filepath.Join(dir, "history", "claude.jsonl"))
	atts := lines(t, filepath.Join(dir, "attempts", "claude.jsonl"))

	if err := s.VerifyProfileIdentity("claude", "B", true); err != nil {
		t.Fatal(err)
	}
	after := getSub(t, s, "claude")
	if after.AccountEmail != nil || after.Plan != nil {
		t.Fatalf("metadata visible after switch: %+v", after)
	}
	// Metadata-only: usage, error, schedule, journals, failures unchanged.
	before.AccountEmail, before.Plan = nil, nil
	bj, _ := json.Marshal(before)
	aj, _ := json.Marshal(after)
	if string(bj) != string(aj) {
		t.Fatalf("usage/schedule changed:\n%s\n%s", bj, aj)
	}
	if s.Failures("claude") != failures ||
		strings.Join(lines(t, filepath.Join(dir, "history", "claude.jsonl")), "\n") != strings.Join(hist, "\n") ||
		strings.Join(lines(t, filepath.Join(dir, "attempts", "claude.jsonl")), "\n") != strings.Join(atts, "\n") {
		t.Fatal("journals/failures changed by verify")
	}

	// A late result from the old account (in flight during the switch) still
	// updates usage as a normal observation but cannot restore the profile.
	t2 := t0.Add(2 * time.Minute)
	mustCommit(t, s, okUpdate("claude", t2, profResult(t2, 40, "A", prof("A", pEmail, "Pro"))))
	sub := getSub(t, s, "claude")
	if sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("late old result restored profile: %+v", sub)
	}
	if sub.Quotas[0].UsedPercent == nil || *sub.Quotas[0].UsedPercent != 40 {
		t.Fatalf("usage should follow the observation: %+v", sub.Quotas)
	}

	// Even a result carrying the new identity's profile cannot unblock this ID,
	// and verifying the old identity again does not revive it.
	if err := s.VerifyProfileIdentity("claude", "A", true); err != nil {
		t.Fatal(err)
	}
	if sub := getSub(t, s, "claude"); sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("re-verify revived: %+v", sub)
	}
	s.Close()

	// Permanent across restart.
	s = profStore(t, dir, true)
	defer s.Close()
	t3 := t0.Add(3 * time.Minute)
	mustCommit(t, s, okUpdate("claude", t3, profResult(t3, 41, "A", prof("A", pEmail, "Pro"))))
	if sub := getSub(t, s, "claude"); sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("restart revived: %+v", sub)
	}
}

func TestVerifyIdentityFallsBackToGoodIdentityAndTemporaryHide(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	defer s.Close()
	// No stored profile: Good.Identity is the baseline; the result Plan ("max")
	// is the only metadata.
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", nil)))

	// Unavailable identity hides metadata, but is not a permanent block.
	if err := s.VerifyProfileIdentity("claude", "", false); err != nil {
		t.Fatal(err)
	}
	if sub := getSub(t, s, "claude"); sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("not hidden: %+v", sub)
	}
	// Same identity re-confirmed: visible again.
	if err := s.VerifyProfileIdentity("claude", "A", true); err != nil {
		t.Fatal(err)
	}
	if sub := getSub(t, s, "claude"); !eqStr(sub.Plan, "max") {
		t.Fatalf("not restored: %+v", sub)
	}
	// Different identity: permanent.
	if err := s.VerifyProfileIdentity("claude", "B", true); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyProfileIdentity("claude", "A", true); err != nil {
		t.Fatal(err)
	}
	if sub := getSub(t, s, "claude"); sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("revived: %+v", sub)
	}
}

func TestTemporaryHidePersistsAndCommitReconfirms(t *testing.T) {
	dir := t.TempDir()
	s := profStore(t, dir, true)
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	if err := s.VerifyProfileIdentity("claude", "", false); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = profStore(t, dir, true)
	defer s.Close()
	if sub := getSub(t, s, "claude"); sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("hide lost on restart: %+v", sub)
	}
	t1 := t0.Add(time.Minute)
	mustCommit(t, s, okUpdate("claude", t1, profResult(t1, 11, "A", prof("A", pEmail, "Pro"))))
	if sub := getSub(t, s, "claude"); !eqStr(sub.AccountEmail, pEmail) {
		t.Fatalf("fresh profile not shown: %+v", sub)
	}
}

func TestVerifyWithoutAnyIdentityNeverBlocks(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	defer s.Close()
	r := result(t0, 10) // no Identity, no Profile
	mustCommit(t, s, okUpdate("claude", t0, r))
	if err := s.VerifyProfileIdentity("claude", "whatever", true); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Minute)
	mustCommit(t, s, okUpdate("claude", t1, profResult(t1, 11, "", &model.Profile{Identity: "whatever", AccountEmail: ptr(pEmail)})))
	if sub := getSub(t, s, "claude"); !eqStr(sub.AccountEmail, pEmail) {
		t.Fatalf("%+v", sub)
	}
}

func TestCredentialGenerationChangeDoesNotLeakCachedProfile(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			account := strings.Repeat("a", 64)
			oldID, newID := account+".old-token-hash", account+".new-token-hash"
			if provider == "claude" {
				oldID, newID = "claude:"+oldID, "claude:"+newID
			}
			s, err := Open(t.TempDir(), []model.Definition{{ID: "sub", Provider: provider, PublishAccountEmail: true}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			mustCommit(t, s, okUpdate("sub", t0, profResult(t0, 10, account, prof(oldID, pEmail, "max"))))
			if err := s.VerifyProfileIdentity("sub", newID, true); err != nil {
				t.Fatal(err)
			}
			if sub := getSub(t, s, "sub"); sub.AccountEmail != nil || sub.Plan != nil || len(sub.Quotas) == 0 {
				t.Fatal("old credential profile leaked")
			}
			mustCommit(t, s, okUpdate("sub", t0.Add(time.Minute), profResult(t0.Add(time.Minute), 11, account, prof(oldID, pEmail, "max"))))
			if sub := getSub(t, s, "sub"); sub.AccountEmail != nil || sub.Plan != nil {
				t.Fatal("late response restored old generation")
			}
			mustCommit(t, s, okUpdate("sub", t0.Add(2*time.Minute), profResult(t0.Add(2*time.Minute), 12, account, prof(newID, pEmail2, "pro"))))
			sub := getSub(t, s, "sub")
			if provider == "codex" && !eqStr(sub.AccountEmail, pEmail2) {
				t.Fatal("authenticated new Codex generation not confirmed")
			}
			if provider == "claude" && (sub.AccountEmail != nil || sub.Plan != nil) {
				t.Fatal("opaque Claude token rebind must remain blocked")
			}
		})
	}
}

func TestVerifyErrors(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	if err := s.VerifyProfileIdentity("nope", "A", true); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("%v", err)
	}
	s.Close()
	if err := s.VerifyProfileIdentity("claude", "A", true); !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
}

func TestCommitIdentityChangeInvalidatesProfileButKeepsUsage(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	defer s.Close()
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	t1 := t0.Add(time.Minute)
	err := s.Commit(okUpdate("claude", t1, profResult(t1, 90, "B", prof("B", pEmail2, "Free"))))
	if !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("%v", err)
	}
	sub := getSub(t, s, "claude")
	if sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("metadata visible: %+v", sub)
	}
	if sub.Error == nil || sub.Error.Code != "source_identity_changed" || *sub.Quotas[0].UsedPercent != 10 {
		t.Fatalf("old usage must be kept with error: %+v", sub)
	}
	// Old account returning afterwards cannot restore the profile.
	t2 := t1.Add(time.Minute)
	if err := s.Commit(okUpdate("claude", t2, profResult(t2, 11, "A", prof("A", pEmail, "Pro")))); err != nil {
		t.Fatal(err)
	}
	if sub := getSub(t, s, "claude"); sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("restored: %+v", sub)
	}
}

func TestProfileIdentityMismatchInResultInvalidates(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	defer s.Close()
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	t1 := t0.Add(time.Minute)
	// Same usage identity, but the profile claims another account.
	mustCommit(t, s, okUpdate("claude", t1, profResult(t1, 20, "A", prof("B", pEmail2, "Free"))))
	sub := getSub(t, s, "claude")
	if sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("mismatch must hide: %+v", sub)
	}
	if *sub.Quotas[0].UsedPercent != 20 || sub.Status != "ok" {
		t.Fatalf("usage should advance: %+v", sub)
	}
}

func TestIdentityChangedFailureInvalidatesProfile(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	defer s.Close()
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	mustCommit(t, s, failUpdate("claude", t0.Add(time.Minute), "source_identity_changed"))
	sub := getSub(t, s, "claude")
	if sub.AccountEmail != nil || sub.Plan != nil || sub.Quotas[0].UsedPercent == nil {
		t.Fatalf("%+v", sub)
	}
	t2 := t0.Add(2 * time.Minute)
	mustCommit(t, s, okUpdate("claude", t2, profResult(t2, 11, "A", prof("A", pEmail, "Pro"))))
	if sub := getSub(t, s, "claude"); sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("restored: %+v", sub)
	}
}

func TestTwoSubscriptionsSameProviderAreIsolated(t *testing.T) {
	dir := t.TempDir()
	defs2 := []model.Definition{
		{ID: "claude-a", Provider: "claude", Product: "code", Source: "claude_code", PublishAccountEmail: true},
		{ID: "claude-b", Provider: "claude", Product: "code", Source: "claude_code", PublishAccountEmail: false},
	}
	s, err := Open(dir, defs2)
	if err != nil {
		t.Fatal(err)
	}
	mustCommit(t, s, okUpdate("claude-a", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	mustCommit(t, s, okUpdate("claude-b", t0, profResult(t0, 20, "B", prof("B", pEmail2, "Team"))))

	a, b := getSub(t, s, "claude-a"), getSub(t, s, "claude-b")
	if !eqStr(a.AccountEmail, pEmail) || !eqStr(a.Plan, "Pro") {
		t.Fatalf("a: %+v", a)
	}
	if b.AccountEmail != nil || !eqStr(b.Plan, "Team") {
		t.Fatalf("b per-definition opt-in: %+v", b)
	}

	// Switch on a only; b keeps its metadata and a's late result stays blocked.
	if err := s.VerifyProfileIdentity("claude-a", "X", true); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Minute)
	mustCommit(t, s, okUpdate("claude-a", t1, profResult(t1, 11, "A", prof("A", pEmail, "Pro"))))
	// b gets a profile from "a"'s identity: only its own identity is compared.
	mustCommit(t, s, okUpdate("claude-b", t1, profResult(t1, 21, "B", prof("B", pEmail2, "Team"))))

	a, b = getSub(t, s, "claude-a"), getSub(t, s, "claude-b")
	if a.AccountEmail != nil || a.Plan != nil {
		t.Fatalf("a: %+v", a)
	}
	if !eqStr(b.Plan, "Team") || *b.Quotas[0].UsedPercent != 21 {
		t.Fatalf("b disturbed: %+v", b)
	}
	s.Close()

	s, err = Open(dir, defs2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, b = getSub(t, s, "claude-a"), getSub(t, s, "claude-b")
	if a.Plan != nil || !eqStr(b.Plan, "Team") {
		t.Fatalf("after restart a=%+v b=%+v", a, b)
	}
}

func TestRemovedDefinitionExcludedFromSnapshotButStateKept(t *testing.T) {
	dir := t.TempDir()
	both := []model.Definition{
		{ID: "claude", Provider: "claude", Product: "code", Source: "claude_code", PublishAccountEmail: true},
		{ID: "codex", Provider: "codex", Product: "cli", Source: "codex", PublishAccountEmail: true},
	}
	s, err := Open(dir, both)
	if err != nil {
		t.Fatal(err)
	}
	mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", prof("A", pEmail, "Pro"))))
	mustCommit(t, s, okUpdate("codex", t0, profResult(t0, 10, "C", prof("C", pEmail2, "Plus"))))
	s.Close()

	s, err = Open(dir, both[:1])
	if err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot(t0.Add(time.Minute))
	if len(snap.Subscriptions) != 1 || snap.Subscriptions[0].ID != "claude" {
		t.Fatalf("%+v", snap.Subscriptions)
	}
	b, _ := json.Marshal(snap)
	if strings.Contains(string(b), pEmail2) || strings.Contains(string(b), "Plus") || strings.Contains(string(b), "codex") {
		t.Fatalf("removed subscription leaked: %s", b)
	}
	if _, ok := s.GetAt("codex", t0); ok {
		t.Fatal("removed id visible")
	}
	if err := s.VerifyProfileIdentity("codex", "C", true); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("%v", err)
	}
	if err := s.Commit(okUpdate("codex", t0, profResult(t0, 1, "C", prof("C", pEmail2, "Plus")))); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("%v", err)
	}
	s.Close()

	// Re-adding restores the untouched state.
	s, err = Open(dir, both)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if sub := getSub(t, s, "codex"); !eqStr(sub.AccountEmail, pEmail2) || !eqStr(sub.Plan, "Plus") {
		t.Fatalf("%+v", sub)
	}
}

func TestOldStateWithoutProfileIsCompatible(t *testing.T) {
	dir := t.TempDir()
	s := profStore(t, dir, true)
	s.Close() // creates the directory layout

	old := `{"version":1,"id":"claude","seq":1,"failures":0,` +
		`"last_attempt_at":"2026-11-04T10:00:00Z","next_attempt_at":"2026-11-04T10:10:00Z",` +
		`"good":{"observed_at":"2026-11-04T10:00:00Z","plan":"max","quotas":[],"metrics":[],"key":"k"}}`
	if err := os.WriteFile(filepath.Join(dir, "state", "claude.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s = profStore(t, dir, true)
	defer s.Close()
	sub := getSub(t, s, "claude")
	if !eqStr(sub.Plan, "max") || sub.AccountEmail != nil || sub.Status != "ok" {
		t.Fatalf("%+v", sub)
	}
	// Verify on a profile-less state with no identity is a no-op.
	if err := s.VerifyProfileIdentity("claude", "A", true); err != nil {
		t.Fatal(err)
	}
	if sub := getSub(t, s, "claude"); !eqStr(sub.Plan, "max") {
		t.Fatalf("%+v", sub)
	}
	// A pre-profile style result (Profile nil) keeps working and keeps Plan.
	t1 := t0.Add(time.Minute)
	mustCommit(t, s, okUpdate("claude", t1, result(t1, 33)))
	sub = getSub(t, s, "claude")
	if !eqStr(sub.Plan, "max") || sub.AccountEmail != nil || *sub.Quotas[0].UsedPercent != 33 {
		t.Fatalf("%+v", sub)
	}
	// Old result JSON (no profile key) decodes with nil Profile.
	var r model.Result
	if err := json.Unmarshal([]byte(`{"ObservedAt":"2026-11-04T10:00:00Z","Plan":"max"}`), &r); err != nil || r.Profile != nil {
		t.Fatalf("%v %+v", err, r.Profile)
	}
}

func TestProfileInputSanitization(t *testing.T) {
	long := strings.Repeat("a", 301-len("@example.test")) + "@example.test"
	okLongEmail := strings.Repeat("a", 300-len("@example.test")) + "@example.test"
	cases := []struct {
		name      string
		email     string
		plan      string
		wantEmail bool
		wantPlan  bool
	}{
		{"valid", pEmail, "Pro", true, true},
		{"email too long", long, "Pro", false, true},
		{"email at max length", okLongEmail, "Pro", true, true},
		{"newline in email", "alice@example.test\nBcc: x@example.test", "Pro", false, true},
		{"trailing newline", pEmail + "\n", "Pro", false, true},
		{"control NUL", "ali\x00ce@example.test", "Pro", false, true},
		{"escape sequence", "\x1b[31malice@example.test", "Pro", false, true},
		{"zero width format char", "ali\u200bce@example.test", "Pro", false, true},
		{"display name form", "Alice <alice@example.test>", "Pro", false, true},
		{"no at sign", "alice.example.test", "Pro", false, true},
		{"invalid utf8", "ali\xffce@example.test", "Pro", false, true},
		{"plan with newline", pEmail, "Pro\nAdmin", true, false},
		{"plan with control", pEmail, "Pro\x07", true, false},
		{"plan too long", pEmail, strings.Repeat("p", 101), true, false},
		{"plan at max length", pEmail, strings.Repeat("p", 100), true, true},
		{"plan unicode runes counted", pEmail, strings.Repeat("я", 100), true, true},
		{"plan unicode over limit", pEmail, strings.Repeat("я", 101), true, false},
		{"plan blank", pEmail, "   ", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			s := profStore(t, dir, true)
			defer s.Close()
			p := &model.Profile{Identity: "A"}
			if c.email != "" {
				p.AccountEmail = ptr(c.email)
			}
			if c.plan != "" {
				p.Plan = ptr(c.plan)
			}
			mustCommit(t, s, okUpdate("claude", t0, profResult(t0, 10, "A", p)))
			sub := getSub(t, s, "claude")
			if (sub.AccountEmail != nil) != c.wantEmail {
				t.Fatalf("email published=%v want %v (%v)", sub.AccountEmail != nil, c.wantEmail, sub.AccountEmail)
			}
			if (sub.Plan != nil) != c.wantPlan {
				t.Fatalf("plan published=%v want %v", sub.Plan != nil, c.wantPlan)
			}
			if sub.AccountEmail != nil && *sub.AccountEmail != c.email {
				t.Fatalf("email altered: %q", *sub.AccountEmail)
			}
			// Rejected values must not reach the persisted state either.
			if !c.wantPlan && strings.Contains(readFile(t, filepath.Join(dir, "state", "claude.json")), `"plan":"`+c.plan) {
				t.Fatal("rejected plan persisted")
			}
		})
	}
}

func TestPersistedUnsanitizedProfileIsFilteredOnView(t *testing.T) {
	dir := t.TempDir()
	s := profStore(t, dir, true)
	s.Close()
	hostile := `{"version":1,"id":"claude","seq":1,"failures":0,` +
		`"good":{"observed_at":"2026-11-04T10:00:00Z","plan":"max","quotas":[],"metrics":[],"key":"k"},` +
		`"profile":{"identity":"A","account_email":"Alice <alice@example.test>","plan":"Pro\u0007"}}`
	if err := os.WriteFile(filepath.Join(dir, "state", "claude.json"), []byte(hostile), 0o600); err != nil {
		t.Fatal(err)
	}
	s = profStore(t, dir, true)
	defer s.Close()
	sub := getSub(t, s, "claude")
	if sub.AccountEmail != nil || sub.Plan != nil {
		t.Fatalf("hostile persisted metadata published: %+v", sub)
	}
}
