package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestModelListsContainOnlyUserAddedModels(t *testing.T) {
	for _, tc := range []struct {
		name, file, wantDefault string
		wantIDs                 []string
	}{
		{name: "new-install", wantIDs: []string{}},
		{name: "legacy-presets", file: `{"accounts":[],"defaultModel":"cline-free/glm-5.2","disabledModels":["cline-pass/glm-5.2"]}`, wantIDs: []string{}},
		{name: "legacy-with-custom", file: `{"accounts":[],"defaultModel":"cline-free/glm-5.2","customModels":["vendor/custom"]}`, wantIDs: []string{"vendor/custom"}, wantDefault: "vendor/custom"},
		{name: "user-added-former-preset", file: `{"accounts":[],"defaultModel":"cline-pass/glm-5.2","customModels":["vendor/custom","cline-pass/glm-5.2"],"disabledModels":["cline-pass/glm-5.2"]}`, wantIDs: []string{"vendor/custom", "cline-pass/glm-5.2"}, wantDefault: "cline-pass/glm-5.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTemporaryPool(t)
			if tc.file != "" {
				if err := os.WriteFile(poolPath, []byte(tc.file), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for reload := 0; reload < 2; reload++ {
				models := allModels()
				ids := make([]string, 0, len(models))
				for _, m := range models {
					ids = append(ids, m.ID)
					if !m.Custom {
						t.Fatalf("unexpected preset model: %+v", m)
					}
				}
				if !reflect.DeepEqual(ids, tc.wantIDs) || getDefaultModel() != tc.wantDefault {
					t.Fatalf("models=%v default=%q, want %v / %q", ids, getDefaultModel(), tc.wantIDs, tc.wantDefault)
				}
				rec := httptest.NewRecorder()
				handleAdminModels(rec, httptest.NewRequest(http.MethodGet, "/admin/api/models", nil))
				var result struct {
					Data struct {
						Models []modelDefinition `json:"models"`
					} `json:"data"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusOK || !reflect.DeepEqual(result.Data.Models, models) {
					t.Fatalf("admin models disagree: %s", rec.Body.String())
				}
				if err := savePool(); err != nil {
					t.Fatal(err)
				}
				pool = nil
			}
		})
	}
}

func TestRequestsUseExplicitOrUserConfiguredModels(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			upstreamTestPool(t)
			var requests atomic.Int64
			modelsSeen := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				modelsSeen <- body.Model
				writeJSON(w, http.StatusOK, map[string]any{
					"id": "test", "model": body.Model,
					"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
				})
			}))
			originalBase := clineAPIBase
			clineAPIBase = server.URL
			t.Cleanup(func() { clineAPIBase = originalBase; server.Close() })
			request := func(model *string) *httptest.ResponseRecorder {
				t.Helper()
				params := map[string]any{"max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
				if model != nil {
					params["model"] = *model
				}
				rec := httptest.NewRecorder()
				if protocol == "anthropic" {
					body, err := json.Marshal(params)
					if err != nil {
						t.Fatal(err)
					}
					handleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))))
				} else {
					resp, err := callClineAPI(context.Background(), params, false)
					if err != nil {
						writeUpstreamError(rec, err)
					} else {
						defer resp.Body.Close()
						handleNonStreamResponse(rec, resp)
					}
				}
				return rec
			}
			empty, whitespace := "", "   "
			for _, model := range []*string{nil, &empty, &whitespace} {
				if rec := request(model); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model is required") {
					t.Fatalf("missing model: status=%d body=%s", rec.Code, rec.Body.String())
				}
			}
			if requests.Load() != 0 {
				t.Fatal("missing model should not contact upstream")
			}
			explicit := "vendor/explicit"
			if rec := request(&explicit); rec.Code != http.StatusOK || <-modelsSeen != explicit {
				t.Fatalf("explicit model failed: %s", rec.Body.String())
			}
			if _, err := addCustomModel("vendor/default"); err != nil {
				t.Fatal(err)
			}
			if rec := request(nil); rec.Code != http.StatusOK || <-modelsSeen != "vendor/default" {
				t.Fatalf("user default failed: %s", rec.Body.String())
			}
			if err := deleteCustomModel("vendor/default"); err != nil {
				t.Fatal(err)
			}
			if rec := request(nil); rec.Code != http.StatusBadRequest || requests.Load() != 2 {
				t.Fatalf("deleted model used as fallback: %s", rec.Body.String())
			}
		})
	}
}
