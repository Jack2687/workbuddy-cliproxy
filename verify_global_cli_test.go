package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// globalProductConfig is a WorkBuddy AI /v3/config under the WorkBuddy UA: the
// CLI models live under data.agents[].models for the "cli" agent, details in
// data.models. The ide agent is unrelated and must never leak into the picker.
const globalProductConfig = `{
	"agents": [
		{"name":"ide","models":["gpt-5.5","gemini-3.1-pro"]},
		{"name":"cli","models":["default-model","gpt-5.5","hy4-preview","glm-5.2","deep-model"]}
	],
	"models": [
		{"id":"default-model","name":"Auto","maxInputTokens":176000,"isDefault":true,"supportsImages":true},
		{"id":"gpt-5.5","name":"GPT-5.5","maxInputTokens":1000000,"supportsImages":true},
		{"id":"hy4-preview","name":"Hy4 preview","maxInputTokens":1000000},
		{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000000},
		{"id":"deep-model","name":"Deep","maxInputTokens":272000}
	]
}`

// ideOnlyConfig is what a CodeBuddy UA gets back: the IDE config with no cli
// agent and an empty model list, so the picker would be empty.
const ideOnlyConfig = `{"agent":{"agents":null},"mcp":{"enableFilterCount":0}}`

// TestVerifyGlobalCliAgentEndToEnd calls the real fetchRemoteModels with a
// global credential against a local stand-in for www.workbuddy.ai. The mock
// returns the cli-agent product config only under the WorkBuddy UA, mirroring
// magpie's finding that the IDE config (no cli agent) comes back otherwise.
// This proves the global path (a) sends the WorkBuddy UA and (b) reads the cli
// agent's model list, in order, with details backfilled from data.models.
func TestVerifyGlobalCliAgentEndToEnd(t *testing.T) {
	var uaSeen []string
	global := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uaSeen = append(uaSeen, r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("User-Agent"), "WorkBuddy/") {
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": json.RawMessage(globalProductConfig)})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": json.RawMessage(ideOnlyConfig)})
	}))
	defer global.Close()

	prev := resolveBase
	resolveBase = func(region string) string {
		if normalizeRegion(region) == regionGlobal {
			return global.URL
		}
		// The cross-realm CN call is best-effort and swallowed; point it at the
		// same mock so the test makes no real network calls.
		return global.URL
	}
	defer func() { resolveBase = prev }()

	sa := &storedAuth{Region: "global"}
	primary, supplements, err := fetchRemoteModels(sa)
	if err != nil && len(primary) == 0 && len(supplements) == 0 {
		t.Fatalf("fetchRemoteModels failed: %v", err)
	}

	fmt.Printf("UA sent to /v3/config: %v\n", uaSeen)
	fmt.Printf("primary models (%d):\n", len(primary))
	for _, m := range primary {
		fmt.Printf("  - %-16s ctx=%d name=%q\n", m.ID, m.MaxInputTokens, m.Name)
	}
	fmt.Printf("supplements: %d\n", len(supplements))

	if len(uaSeen) == 0 {
		t.Fatalf("no /v3/config request observed")
	}
	if !strings.Contains(uaSeen[0], "WorkBuddy/") {
		t.Fatalf("global /v3/config did not use the WorkBuddy UA, got %q", uaSeen[0])
	}
	want := []string{"default-model", "gpt-5.5", "hy4-preview", "glm-5.2", "deep-model"}
	if len(primary) != len(want) {
		t.Fatalf("primary = %d models, want %d (cli agent list)", len(primary), len(want))
	}
	for i, id := range want {
		if primary[i].ID != id {
			t.Errorf("primary[%d].ID = %q, want %q", i, primary[i].ID, id)
		}
		if primary[i].MaxInputTokens == 0 {
			t.Errorf("model %q lost detail backfill from data.models", id)
		}
	}
	if hasID(primary, "gemini-3.1-pro") {
		t.Errorf("ide-only model gemini-3.1-pro leaked into the picker")
	}
}

func hasID(ms []upstreamModel, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestVerifyCodeBuddyUAGetsNoCliAgent is the counter-proof: the same mock, but
// with the legacy CodeBuddy UA on the global /v3/config, returns only the IDE
// config (no cli agent) and therefore an empty model list. This is exactly the
// gap magpie documents and what the WorkBuddy UA switch fixes — it shows the
// change is load-bearing, not cosmetic.
func TestVerifyCodeBuddyUAGetsNoCliAgent(t *testing.T) {
	global := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("User-Agent"), "WorkBuddy/") {
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": json.RawMessage(globalProductConfig)})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": json.RawMessage(ideOnlyConfig)})
	}))
	defer global.Close()

	prev := resolveBase
	resolveBase = func(region string) string { return global.URL }
	defer func() { resolveBase = prev }()

	// Force the legacy CodeBuddy UA by pointing at the CN branch's header logic:
	// a global credential of "cn" region keeps clientUA, and only the global
	// realm switches to clientUAGlobal. We assert the opposite — that with the
	// WorkBuddy UA absent, the cli agent is unreachable.
	sa := &storedAuth{Region: "global"}
	primary, _, err := fetchRemoteModelsWithUA(sa, global.URL, clientUA)
	if err != nil && len(primary) == 0 {
		t.Logf("expected empty catalog under CodeBuddy UA: %v", err)
	}
	if len(primary) != 0 {
		t.Fatalf("CodeBuddy UA returned %d models, want 0 (no cli agent in IDE config): %v", len(primary), idsOnly(primary))
	}
}

// fetchRemoteModelsWithUA is a thin shim that runs discovery against a fixed
// base with a forced User-Agent on the config request, to demonstrate the
// UA-dependent behaviour without a build tag.
func fetchRemoteModelsWithUA(sa *storedAuth, base, ua string) ([]upstreamModel, []upstreamModel, error) {
	prev := resolveBase
	resolveBase = func(string) string { return base }
	defer func() { resolveBase = prev }()
	// Override the global UA constant temporarily so the global branch emits the
	// supplied UA instead of clientUAGlobal.
	prevUA := clientUAGlobal
	clientUAGlobal = ua
	defer func() { clientUAGlobal = prevUA }()
	return fetchRemoteModels(sa)
}

func idsOnly(ms []upstreamModel) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}
