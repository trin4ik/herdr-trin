package store

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestLocalProfileOnlyUpdatesMetadataAndDeduplicates(t *testing.T) {
	s := profStore(t, t.TempDir(), true)
	defer s.Close()
	mustCommit(t, s, okUpdate("claude", t0, result(t0, 20)))
	before := getSub(t, s, "claude")
	failures := s.Failures("claude")
	p := prof("native-generation", pEmail, "max")
	if err := s.CommitLocalProfile("claude", *p); err != nil {
		t.Fatal(err)
	}
	after := getSub(t, s, "claude")
	if !eqStr(after.AccountEmail, pEmail) || !eqStr(after.Plan, "max") {
		t.Fatal("missing local metadata")
	}
	after.AccountEmail, after.Plan = before.AccountEmail, before.Plan
	if !reflect.DeepEqual(before, after) || s.Failures("claude") != failures {
		t.Fatal("local profile modified usage")
	}
	statBefore, err := os.Stat(s.statePath("claude"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitLocalProfile("claude", *p); err != nil {
		t.Fatal(err)
	}
	statAfter, err := os.Stat(s.statePath("claude"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(statBefore, statAfter) {
		t.Fatal("unchanged profile rewrote state")
	}
	if err := s.CommitLocalProfile("claude", *prof("other", pEmail2, "pro")); !errors.Is(err, ErrIdentityChanged) {
		t.Fatal("different native generation accepted")
	}
	if err := s.CommitLocalProfile("missing", *p); !errors.Is(err, ErrUnknownID) {
		t.Fatal("unknown subscription accepted")
	}
}
