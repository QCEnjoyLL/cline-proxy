package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCheckUninstalledModelDoesNotInstallOrChangeConfig(t *testing.T) {
	p := seedReliabilityPool(t)
	p.Accounts[0].AccessToken = "workos:cached"
	p.Accounts[0].ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	before, _ := os.ReadFile(poolPath)
	calls := 0
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "vendor/not-installed" || body["max_tokens"] != float64(probeMaxTokens) {
			t.Fatalf("wrong request: %v", body)
		}
		if body["provider"] != nil || body["providerOptions"] != nil {
			t.Fatal("availability check unexpectedly enumerated channels")
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 90*time.Second {
			t.Fatal("missing bounded request deadline")
		}
		return reliabilityResponse(200, `{"data":{"choices":[{"message":{"content":"OK"}}]}}`), nil
	})
	result, err := checkModelAvailability(context.Background(), "vendor/not-installed")
	if err != nil || result == nil || result.ModelID != "vendor/not-installed" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if calls != 1 {
		t.Fatalf("sent %d requests, want 1", calls)
	}
	after, _ := os.ReadFile(poolPath)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(p.CustomModels, []string{"vendor/model"}) || len(p.PerModel) != 1 {
		t.Fatal("check mutated configuration")
	}
}

func TestCheckModelHonorsConfiguredRouting(t *testing.T) {
	p := seedReliabilityPool(t)
	p.Accounts[0].AccessToken = "workos:cached"
	p.Accounts[0].ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	p.PerModel["vendor/model"] = ModelUpstream{Redirect: "vendor/real", Pipeline: pipelinePlanner, Upstreams: []string{"deepseek"}}
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "vendor/real" || !reflect.DeepEqual(nestedMap(body, "providerOptions", "gateway")["only"], []any{"deepseek"}) {
			t.Fatalf("routing differs from configuration: %v", body)
		}
		return reliabilityResponse(200, `{"choices":[{"message":{"content":"OK"}}]}`), nil
	})
	if _, err := checkModelAvailability(context.Background(), "vendor/model"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckModelRejectsFalseSuccessAndReportsFailures(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body, want string
	}{
		{"html", 200, "<html>gateway</html>", "JSON"},
		{"error", 200, `{"error":"bad"}`, "有效文本"},
		{"empty", 200, `{"choices":[{"message":{"content":" "},"finish_reason":"length"}]}`, "有效文本"},
		{"auth", 403, `{}`, "认证失败"},
		{"quota", 402, `{}`, "额度"},
		{"missing", 404, `{}`, "不存在"},
		{"rate", 429, `{}`, "限流"},
		{"server", 503, `<html>private upstream details</html>`, "HTTP 503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := seedReliabilityPool(t)
			p.Accounts[0].AccessToken = "workos:cached"
			p.Accounts[0].ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
			mockReliabilityHTTP(t, func(*http.Request) (*http.Response, error) { return reliabilityResponse(tc.status, tc.body), nil })
			_, err := checkModelAvailability(context.Background(), "vendor/new")
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "private") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := checkModelAvailability(ctx, "vendor/new"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestAdminModelCheckAsyncValidationAndAuth(t *testing.T) {
	old := modelCheckJobs
	modelCheckJobs = newProbeJobStore(func(context.Context, string) (*probeResult, error) {
		return &probeResult{ModelID: "vendor/new", LatencyMS: 15}, nil
	})
	t.Cleanup(func() { modelCheckJobs = old })
	for _, body := range []string{`{}`, `{"modelId":"bad model"}`, `{"modelId":"a|b"}`, `{`} {
		w := httptest.NewRecorder()
		handleAdminModelCheck(w, httptest.NewRequest("POST", "/admin/api/models/check", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("bad input accepted: %s", body)
		}
	}
	w := httptest.NewRecorder()
	handleAdminModelCheck(w, httptest.NewRequest("POST", "/admin/api/models/check", strings.NewReader(`{"modelId":"vendor/new"}`)))
	if w.Code != 202 {
		t.Fatalf("start status %d", w.Code)
	}
	var response struct {
		Data probeJob `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	job := awaitProbeJob(t, modelCheckJobs, response.Data.ID)
	if job.Status != "done" {
		t.Fatalf("job: %+v", job)
	}
	w = httptest.NewRecorder()
	handleAdminModelCheck(w, httptest.NewRequest("GET", "/admin/api/models/check?jobId="+job.ID, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"latencyMs":15`) {
		t.Fatal(w.Body.String())
	}
	mux := http.NewServeMux()
	registerAdminRoutes(mux)
	for _, method := range []string{"GET", "POST"} {
		w = httptest.NewRecorder()
		requireAdminAuth(mux).ServeHTTP(w, httptest.NewRequest(method, "/admin/api/models/check", nil))
		if w.Code != 401 {
			t.Fatalf("%s is not protected: %d", method, w.Code)
		}
	}
}
