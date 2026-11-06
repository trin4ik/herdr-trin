package store

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestProviderCannotBeRelabeledUnderAnExistingSubscriptionID(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.Commit(okUpdate("codex", t0, result(t0, 25))); err != nil {
		t.Fatal(err)
	}
	s.Close()
	d := defs()
	d[1].Provider = "ollama"
	if changed, err := Open(dir, d); !errors.Is(err, ErrDefinitionChanged) {
		if changed != nil {
			changed.Close()
		}
		t.Fatalf("old account data relabeled as another provider: %v", err)
	}
}

func TestAccountSwitchPreservesOldGoodButMarksIdentityError(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	first := result(t0, 25)
	first.Identity = "first-account-fingerprint"
	if err := s.Commit(okUpdate("codex", t0, first)); err != nil {
		t.Fatal(err)
	}
	second := result(t0.Add(time.Minute), 90)
	second.Identity = "different-account-fingerprint"
	if err := s.Commit(okUpdate("codex", t0.Add(time.Minute), second)); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("account switch accepted: %v", err)
	}
	sub, _ := s.GetAt("codex", t0.Add(time.Minute))
	if !sub.Stale || sub.Error == nil || sub.Error.Code != "source_identity_changed" || *sub.Quotas[0].UsedPercent != 25 {
		t.Fatal("last-good was replaced by another account")
	}
}

func TestStateTrailingClosingBracketIsRejected(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.Commit(okUpdate("codex", t0, result(t0, 25))); err != nil {
		t.Fatal(err)
	}
	path := s.statePath("codex")
	s.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("]")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(dir, defs()); !errors.Is(err, ErrCorrupt) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal("trailing invalid state data accepted")
	}
}
