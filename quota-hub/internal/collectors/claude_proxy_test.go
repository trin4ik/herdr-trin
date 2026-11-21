package collectors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quota-hub/internal/model"
)

func TestClaudeProxyIndependentContainersAndFailures(t *testing.T) {
	const tokenA, tokenB = "synthetic-token-a", "synthetic-token-b"
	t.Setenv("TEST_CLAUDE_PROXY_TOKEN_A", tokenA)
	t.Setenv("TEST_CLAUDE_PROXY_TOKEN_B", tokenB)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	bodyA := `{"schema_version":1,"instance_id":"claude-a","status":"ok","observed_at":"2026-11-21T11:59:00Z","windows":[{"id":"five_hour","used_percent":42.5,"reset_at":"2026-11-21T13:00:00Z"},{"id":"seven_day","used_percent":null}]}`
	bodyB := `{"schema_version":1,"instance_id":"claude-b","status":"ok","observed_at":"2026-11-21T11:58:00Z","windows":[{"id":"five_hour","used_percent":12}]}`
	statusA, callsA := http.StatusOK, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsA++
		if r.Header.Get("Authorization") != "Bearer "+tokenA {
			t.Errorf("missing dedicated bearer")
		}
		w.WriteHeader(statusA)
		_, _ = w.Write([]byte(bodyA))
	}))
	defer server.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tokenB {
			t.Errorf("container B received wrong token")
		}
		_, _ = w.Write([]byte(bodyB))
	}))
	defer serverB.Close()
	makeCollector := func(id, integration, endpoint, env string) Collector {
		spec := Spec{Definition: model.Definition{ID: id, Provider: ProviderClaude, Source: SourceClaudeProxy},
			IntegrationID: integration, ClaudeProxyURL: endpoint + "/v1/subscription-limits", ClaudeProxyTokenEnv: env}
		c, err := New(spec, Options{Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := makeCollector("claude-a", "container-a", server.URL, "TEST_CLAUDE_PROXY_TOKEN_A")
	c2 := makeCollector("claude-b", "container-b", serverB.URL, "TEST_CLAUDE_PROXY_TOKEN_B")
	result, err := c.Fetch(context.Background())
	if err != nil || !result.ObservedAt.Equal(time.Date(2026, 11, 21, 11, 59, 0, 0, time.UTC)) ||
		len(result.Quotas) != 2 || result.Quotas[0].ID != "five_hour" ||
		result.Quotas[0].UsedPercent == nil || *result.Quotas[0].UsedPercent != 42.5 ||
		result.Quotas[1].UsedPercent != nil {
		t.Fatalf("unexpected container A result %+v, %v", result, err)
	}
	result2, err := c2.Fetch(context.Background())
	if err != nil || result2.Quotas[0].ID != "five_hour" || *result2.Quotas[0].UsedPercent != 12 ||
		result2.ObservedAt.Equal(result.ObservedAt) {
		t.Fatalf("unexpected independent container B result %+v, %v", result2, err)
	}
	statusA = http.StatusUnauthorized
	if _, err := c.Fetch(context.Background()); err == nil || callsA != 2 {
		t.Fatalf("401 should fail without refresh/retry; calls=%d err=%v", callsA, err)
	}
	statusA = http.StatusOK
	bodyA = `{"schema_version":1,"instance_id":"claude-a","status":"no_data","windows":[]}`
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("no_data must be unavailable")
	}
	bodyA = `{"schema_version":1,"instance_id":"claude-a","status":"ok","observed_at":"2026-11-21T11:59:00Z","windows":[{"id":"five_hour","used_percent":1}]} trailing`
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("trailing JSON must be rejected")
	}
	bodyA = `{"schema_version":1,"instance_id":"claude-a","status":"ok","observed_at":"2026-11-21T11:59:00Z","windows":[{"id":"five_hour"}]}`
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("missing used_percent must be rejected")
	}
	bodyA = `{"schema_version":1,"instance_id":"claude-a","status":"ok","observed_at":"2026-11-21T11:59:00Z","windows":[{"id":"five_hour","used_percent":101}]}`
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("out-of-bounds percentage must be rejected")
	}
	bodyA = `{"schema_version":1,"instance_id":"claude-a","status":"ok","observed_at":"2026-11-21T11:59:00Z","windows":[{"id":"five_hour","used_percent":1}]}` +
		strings.Repeat(" ", maxObsBytes)
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("oversized body must be rejected")
	}
}
