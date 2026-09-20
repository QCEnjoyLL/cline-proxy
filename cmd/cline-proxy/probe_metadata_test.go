package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This response represents a successful planner completion, including a provider
// metadata block before gateway.routing (as in the reported DeepSeek response).
const plannerProbeCompletion = `{"data":{"choices":[{"message":{"content":"Hi! How can I help you today?","provider_metadata":{"deepseek":{"promptCacheHitTokens":0},"gateway":{"routing":{"finalProvider":"deepseek","fallbacksAvailable":["deepseek","alibaba","invalid slug"],"canonicalSlug":"deepseek/deepseek-v4.1-flash"}}}}}]}}`

func TestPlannerProbeFallsBackToMetadataWhenFilterIgnored(t *testing.T) {
	p := seedReliabilityPool(t)
	p.Accounts[0].AccessToken = "workos:cached"
	p.Accounts[0].ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	p.PerModel["vendor/model"] = ModelUpstream{Pipeline: pipelinePlanner, Upstreams: []string{"deepseek"}}
	calls := 0
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		only := nestedMap(body, "providerOptions", "gateway")["only"]
		want := "deepseek"
		if calls == 2 {
			want = probeSentinel
		}
		if !reflect.DeepEqual(only, []any{want}) {
			t.Fatalf("request %d filter = %v", calls, only)
		}
		return reliabilityResponse(200, plannerProbeCompletion), nil
	})
	result, err := executeUpstreamProbe(context.Background(), "vendor/model")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("unexpected retries: %d", calls)
	}
	if result.Pipeline != pipelinePlanner || result.Provider != "deepseek" {
		t.Fatalf("lost routing: %+v", result)
	}
	if !reflect.DeepEqual(result.Observed, []string{"deepseek", "alibaba"}) {
		t.Fatalf("metadata fallback: %v", result.Observed)
	}
	if len(result.Available) != 0 {
		t.Fatal("partial metadata mislabeled as exhaustive list")
	}
	if !strings.Contains(result.Note, "模型已正常响应") || !strings.Contains(result.Note, "不能据此确认严格钉住生效") {
		t.Fatalf("missing explanation: %s", result.Note)
	}
	if strings.Contains(result.Note, "Hi!") || strings.Contains(result.Note, "choices") {
		t.Fatal("successful completion dumped into error message")
	}
	entry := p.PerModel["vendor/model"]
	if !reflect.DeepEqual(entry.Observed, result.Observed) || entry.LastProvider != "deepseek" {
		t.Fatalf("metadata not persisted: %+v", entry)
	}
	entry.Upstreams = nil
	entry.Exclude = []string{"alibaba"}
	if prefs := buildUpstreamPrefs(&entry); prefs != nil {
		t.Fatalf("partial observations used as complete allowlist: %v", prefs)
	}
}

func TestProbeEnumerationRetainsCompleteListAndDirectSemantics(t *testing.T) {
	for _, pipeline := range []string{pipelinePlanner, pipelineDirect} {
		t.Run(pipeline, func(t *testing.T) {
			p := seedReliabilityPool(t)
			p.Accounts[0].AccessToken = "workos:cached"
			p.Accounts[0].ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
			calls := 0
			mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if pipeline == pipelinePlanner {
						return reliabilityResponse(200, plannerProbeCompletion), nil
					}
					return reliabilityResponse(200, `{"provider":"DeepInfra","choices":[{"message":{"content":"hi"}}]}`), nil
				}
				return reliabilityResponse(400, `{"error":{"metadata":{"available_providers":["deepseek","deepinfra","alibaba"]}}}`), nil
			})
			result, err := probeModelUpstreams("vendor/model")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.Available, []string{"deepseek", "deepinfra", "alibaba"}) {
				t.Fatalf("complete list lost: %+v", result)
			}
			if pipeline == pipelineDirect && len(result.Observed) > 0 {
				t.Fatal("invented slug from direct display name")
			}
		})
	}
}

func TestPlannerMetadataDoesNotInventCandidatesFromProviderBlocks(t *testing.T) {
	routing := parseUpstreamRouting([]byte(`{"data":{"choices":[{"message":{"provider_metadata":{"deepseek":{"tokens":1}}}}]}}`))
	if got := observedProviders(routing); len(got) != 0 {
		t.Fatalf("provider metadata key treated as routing proof: %v", got)
	}
}

func TestSaveUpstreamPreservesProbeEvidence(t *testing.T) {
	p := seedReliabilityPool(t)
	p.PerModel["vendor/model"] = ModelUpstream{Pipeline: pipelinePlanner, Observed: []string{"deepseek"}, LastProvider: "deepseek"}
	post := upstreamSaveRequest{ModelID: "vendor/model", Upstreams: []string{"deepseek"}}
	data, _ := json.Marshal(post)
	rec := httptest.NewRecorder()
	handleAdminUpstreamSave(rec, httptest.NewRequest("POST", "/admin/api/upstreams/save", strings.NewReader(string(data))))
	if rec.Code != 200 {
		t.Fatalf("save status: %d", rec.Code)
	}
	entry := p.PerModel["vendor/model"]
	if entry.LastProvider != "deepseek" || len(entry.Observed) != 1 {
		t.Fatalf("save erased evidence: %+v", entry)
	}
}
