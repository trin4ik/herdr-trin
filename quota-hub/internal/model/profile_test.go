package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmailNormalization(t *testing.T) {
	for _, s := range []string{"", "username", "display <a@example.test>", "a@example.test\n", "a\x00@example.test", "a\u200b@example.test", strings.Repeat("x", 301) + "@example.test", string([]byte{255})} {
		if Email(s) != nil {
			t.Fatal("invalid email accepted")
		}
	}
	if p := Email("  a@example.test  "); p == nil || *p != "a@example.test" {
		t.Fatal("trim")
	}
}

func TestV1LegacySubscription(t *testing.T) {
	var s Snapshot
	if err := json.Unmarshal([]byte(`{"schema_version":1,"subscriptions":[{"id":"old","plan":"prolite"}]}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.SchemaVersion != SchemaVersion || s.Subscriptions[0].AccountEmail != nil {
		t.Fatal("legacy contract")
	}
	b, err := json.Marshal(s)
	if err != nil || !strings.Contains(string(b), `"account_email":null`) {
		t.Fatal("nullable contract")
	}
}
