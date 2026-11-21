package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"quota-hub/internal/model"
)

func TestUsageFieldsPersistAndCopy(t *testing.T) {
	dir := t.TempDir()
	provider, modelID := "anthropic", "fable"
	definitions := defs()
	definitions[0].OpenCodeProvider = &provider
	definitions[0].DisplayName = "Claude Proxy A"
	definitions[0].VerifiedPlan = "pro"
	definitions[0].VerifiedAccountEmail = "account@example.test"
	s, err := Open(dir, definitions)
	if err != nil {
		t.Fatal(err)
	}
	if sub := s.Snapshot(t0).Subscriptions[0]; sub.Plan == nil || *sub.Plan != "pro" || sub.AccountEmail != nil {
		t.Fatalf("configured plan must be independent of measured quota and email: %+v", sub)
	}
	r := result(t0, 20)
	r.Quotas[0].Model = &modelID
	if err := s.Commit(okUpdate("claude", t0, r)); err != nil {
		t.Fatal(err)
	}
	sub := s.Snapshot(t0).Subscriptions[0]
	if sub.OpenCodeProvider == nil || *sub.OpenCodeProvider != provider ||
		sub.DisplayName == nil || *sub.DisplayName != "Claude Proxy A" ||
		sub.Quotas[0].Model == nil || *sub.Quotas[0].Model != modelID {
		t.Fatalf("%+v", sub)
	}
	definitions[0].PublishAccountEmail = true
	if sub := view(&entry{def: definitions[0]}, t0); sub.AccountEmail == nil || *sub.AccountEmail != "account@example.test" {
		t.Fatal("verified address did not appear with publication consent")
	}
	wire, err := json.Marshal(s.Snapshot(t0))
	if err != nil || !strings.Contains(string(wire), `"opencode_provider":"anthropic"`) ||
		!strings.Contains(string(wire), `"display_name":"Claude Proxy A"`) || !strings.Contains(string(wire), `"plan":"pro"`) ||
		!strings.Contains(string(wire), `"model":"fable"`) {
		t.Fatalf("%s %v", wire, err)
	}
	*sub.OpenCodeProvider, *sub.Quotas[0].Model = "changed", "changed"
	*sub.DisplayName = "changed"
	s.Close()
	s, err = Open(dir, definitions)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sub, _ = s.Get("claude")
	if sub.OpenCodeProvider == nil || *sub.OpenCodeProvider != provider ||
		sub.DisplayName == nil || *sub.DisplayName != "Claude Proxy A" ||
		sub.Quotas[0].Model == nil || *sub.Quotas[0].Model != modelID {
		t.Fatalf("%+v", sub)
	}
	history := readFile(t, filepath.Join(dir, "history", "claude.jsonl"))
	if !strings.Contains(history, `"opencode_provider":"anthropic"`) || !strings.Contains(history, `"model":"fable"`) {
		t.Fatal(history)
	}
}

func TestOldUsageFieldsRemainOptional(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.Commit(okUpdate("claude", t0, result(t0, 20))); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// No quota.model in this last-good; definitions also predate the new field.
	b, err := os.ReadFile(filepath.Join(dir, "state", "claude.json"))
	if err != nil || strings.Contains(string(b), `"model"`) {
		t.Fatalf("%s %v", b, err)
	}
	s = open(t, dir)
	defer s.Close()
	sub, _ := s.Get("claude")
	if sub.OpenCodeProvider != nil || len(sub.Quotas) != 1 || sub.Quotas[0].Model != nil {
		t.Fatalf("%+v", sub)
	}
	var old model.Snapshot
	if err := json.Unmarshal([]byte(`{"schema_version":1,"subscriptions":[{"id":"old","quotas":[{"id":"all"}]}]}`), &old); err != nil ||
		old.Subscriptions[0].OpenCodeProvider != nil || old.Subscriptions[0].Quotas[0].Model != nil {
		t.Fatalf("%+v %v", old, err)
	}
}
