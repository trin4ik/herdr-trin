package collectors

import (
	"encoding/json"
	"testing"
	"time"

	"quota-hub/internal/model"
)

func TestUsageProvider(t *testing.T) {
	for _, tc := range []struct {
		provider, source, integration, want string
	}{
		{ProviderCodex, SourceNativeCodexOAuth, "", "openai"},
		{ProviderClaude, SourceOpenCodeObserved, "", "anthropic"},
		{ProviderClaude, "native_claude_oauth", "", "anthropic"},
		{ProviderOllama, SourceOpenCodeKey, "ollama-cloud", "ollama-cloud"},
		{ProviderKimi, SourceOpenCodeKey, "kimi-code-plan-cn", "kimi-code-plan-cn"},
		{ProviderKimi, SourceOpenCodeKey, "kimi-code-plan-global", "kimi-code-plan-global"},
		{ProviderOpenCode, SourceOpenCodeConsole, "opencode", "opencode"},
		{"other", "other", "", ""},
	} {
		t.Run(tc.provider+"/"+tc.integration, func(t *testing.T) {
			spec := Spec{Definition: model.Definition{Provider: tc.provider, Source: tc.source}, IntegrationID: tc.integration}
			got := spec.UsageProvider()
			if tc.want == "" {
				if got != nil {
					t.Fatal(*got)
				}
			} else if got == nil || *got != tc.want {
				t.Fatalf("got %v want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeScopedModelField(t *testing.T) {
	for _, tc := range []struct {
		model, want string
	}{
		{`{"id":"fable","display_name":"Fable model"}`, "fable"},
		{`{"id":"fable"}`, "fable"},
		{`{"display_name":"Fable"}`, "Fable"},
	} {
		// Use the wire format already accepted by the native OAuth collector.
		var top map[string]json.RawMessage
		raw := `{"limits":[{"kind":"weekly_scoped","percent":20,"scope":{"model":` + tc.model + `}}]}`
		if err := json.Unmarshal([]byte(raw), &top); err != nil {
			t.Fatal(err)
		}
		quotas, ok := parseClaudeLimits(top, time.Now())
		if !ok || len(quotas) != 1 || quotas[0].Model == nil || *quotas[0].Model != tc.want {
			t.Fatalf("%s: %+v", raw, quotas)
		}
	}
}
