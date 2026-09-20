package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type reliabilityTransport func(*http.Request) (*http.Response, error)

func (f reliabilityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockReliabilityHTTP(t *testing.T, fn reliabilityTransport) {
	t.Helper()
	previous := httpClient
	httpClient = &http.Client{Transport: fn}
	t.Cleanup(func() { httpClient = previous })
}
func reliabilityResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const refreshedCredentials = `{"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresAt":4102444800000}}`

func blockPoolWrites(t *testing.T) {
	t.Helper()
	previous, _ := poolSaveBlocked.Load().(string)
	poolSaveBlocked.Store("simulated storage failure")
	t.Cleanup(func() { poolSaveBlocked.Store(previous) })
}
func seedReliabilityPool(t *testing.T) *AccountPool {
	t.Helper()
	useTemporaryPool(t)
	p := loadPool()
	p.Accounts = []*Account{{AccountID: "test-account", Email: "test@example.invalid", Status: "active", RefreshToken: "old-refresh"}}
	p.Keys = []string{"test-key"}
	p.CustomModels = []string{"vendor/model"}
	p.DefaultModel = "vendor/model"
	p.CooldownMinutes = 12
	p.PerModel = map[string]ModelUpstream{"vendor/model": {Upstreams: []string{"provider"}, PinMode: "strict"}}
	p.ProxyConfig = defaultProxyConfig()
	if err := savePool(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDeleteAllAccountsPreservesConfiguration(t *testing.T) {
	p := seedReliabilityPool(t)
	before := *p
	rec := httptest.NewRecorder()
	handleAdminDeleteAll(rec, httptest.NewRequest("POST", "/admin/api/accounts/delete-all", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	pool = nil
	after := loadPool()
	before.Accounts = []*Account{}
	before.CurrentIdx = 0
	if !reflect.DeepEqual(before, *after) {
		t.Fatalf("unrelated configuration changed: %+v", after)
	}
	if !validAPIKey(after.Keys, "test-key") || validAPIKey(after.Keys, "") {
		t.Fatal("account deletion changed API authentication")
	}
}

func TestManagementMutationsRollbackOnStorageFailure(t *testing.T) {
	cases := []struct {
		name, path, body string
		handler          http.HandlerFunc
	}{
		{"delete-account", "/accounts/delete", `{"accountId":"test-account"}`, handleAdminAccountDelete},
		{"delete-all", "/accounts/delete-all", `{}`, handleAdminDeleteAll},
		{"generate-key", "/keys/generate", `{}`, handleAdminGenerateKey},
		{"delete-key", "/keys/delete", `{"key":"test-key"}`, handleAdminDeleteKey},
		{"disable", "/accounts/disable", `{"accountId":"test-account"}`, handleAdminAccountDisable},
		{"upstream-save", "/upstreams/save", `{"modelId":"vendor/model","upstreams":["other"]}`, handleAdminUpstreamSave},
		{"upstream-delete", "/upstreams/delete", `{"modelId":"vendor/model"}`, handleAdminUpstreamDelete},
		{"add-account", "/accounts/add", `{"email":"added@example.invalid","refreshToken":"old"}`, handleAdminAccountAdd},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := seedReliabilityPool(t)
			before, _ := json.Marshal(p)
			diskBefore, _ := os.ReadFile(poolPath)
			blockPoolWrites(t)
			mockReliabilityHTTP(t, func(*http.Request) (*http.Response, error) {
				return reliabilityResponse(200, refreshedCredentials), nil
			})
			rec := httptest.NewRecorder()
			tc.handler(rec, httptest.NewRequest("POST", "/admin/api"+tc.path, strings.NewReader(tc.body)))
			if rec.Code != 500 {
				t.Fatalf("expected failure, got %d: %s", rec.Code, rec.Body.String())
			}
			after, _ := json.Marshal(loadPool())
			diskAfter, _ := os.ReadFile(poolPath)
			if string(before) != string(after) || string(diskBefore) != string(diskAfter) {
				t.Fatal("failed mutation changed memory or disk")
			}
		})
	}
}

func TestImportedCredentialsKeepRotation(t *testing.T) {
	for _, route := range []string{"batch", "sso"} {
		t.Run(route, func(t *testing.T) {
			useTemporaryPool(t)
			mockReliabilityHTTP(t, func(*http.Request) (*http.Response, error) {
				return reliabilityResponse(200, refreshedCredentials), nil
			})
			rec := httptest.NewRecorder()
			if route == "batch" {
				handleBatchImport(rec, httptest.NewRequest("POST", "/admin/api/batch-import", strings.NewReader(`{"tokens":[{"refreshToken":"old-refresh","email":"test@example.invalid"}]}`)))
			} else {
				handleSSOImport(rec, httptest.NewRequest("POST", "/admin/api/sso/import", strings.NewReader(`{"ssoCookies":"workos:old-refresh"}`)))
			}
			if rec.Code != 200 {
				t.Fatal(rec.Body.String())
			}
			pool = nil
			p := loadPool()
			if len(p.Accounts) != 1 || p.Accounts[0].RefreshToken != "new-refresh" {
				t.Fatal("rotated refresh token not persisted")
			}
		})
	}
}

func TestRefreshFailureClassificationAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"network", 0, "", false}, {"outage", 503, `{}`, false}, {"limited", 429, `{}`, false},
		{"malformed", 200, `{}`, false}, {"unauthorized", 401, `{}`, true}, {"revoked", 400, `{"error":"invalid_grant"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := seedReliabilityPool(t)
			acc := p.Accounts[0]
			mockReliabilityHTTP(t, func(*http.Request) (*http.Response, error) {
				if tc.status == 0 {
					return nil, errors.New("connection reset")
				}
				return reliabilityResponse(tc.status, tc.body), nil
			})
			if err := refreshAccountToken(acc); err == nil {
				t.Fatal("expected error")
			}
			if (acc.Status == "expired") != tc.permanent {
				t.Fatalf("incorrect account status %q", acc.Status)
			}
			if !tc.permanent {
				if pickAccount("vendor/model") == nil {
					t.Fatal("transient failure permanently removed account")
				}
				httpClient.Transport = reliabilityTransport(func(*http.Request) (*http.Response, error) {
					return reliabilityResponse(200, refreshedCredentials), nil
				})
				if token, err := ensureAccountToken(acc); err != nil || token != "workos:new-access" {
					t.Fatalf("failed to recover: %s %v", token, err)
				}
			}
		})
	}
}

func TestConcurrentAndLate401RefreshUseOneRotation(t *testing.T) {
	p := seedReliabilityPool(t)
	acc := p.Accounts[0]
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	mockReliabilityHTTP(t, func(*http.Request) (*http.Response, error) {
		if calls.Add(1) != 1 {
			return reliabilityResponse(401, `{}`), nil
		}
		close(entered)
		<-release
		return reliabilityResponse(200, refreshedCredentials), nil
	})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := ensureAccountToken(acc)
			if err != nil || token != "workos:new-access" {
				t.Errorf("refresh: token=%s error=%v", token, err)
			}
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	if _, err := accountToken(acc, true, "workos:old-access"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || acc.Status != "active" {
		t.Fatalf("refreshes=%d status=%s", calls.Load(), acc.Status)
	}
}

func TestRotatedCredentialWriteFailureCanRetryWithoutRotatingAgain(t *testing.T) {
	p := seedReliabilityPool(t)
	acc := p.Accounts[0]
	var calls atomic.Int32
	mockReliabilityHTTP(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return reliabilityResponse(200, refreshedCredentials), nil
	})
	blockPoolWrites(t)
	if _, err := ensureAccountToken(acc); err == nil {
		t.Fatal("missing storage error")
	}
	if acc.RefreshToken != "new-refresh" {
		t.Fatal("must retain credentials already rotated upstream")
	}
	poolSaveBlocked.Store("")
	if _, err := ensureAccountToken(acc); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("saving credentials should not rotate again")
	}
	pool = nil
	if loadPool().Accounts[0].RefreshToken != "new-refresh" {
		t.Fatal("new credential did not survive reload")
	}
}

func TestHeaderReplacementSurvivesReload(t *testing.T) {
	seedReliabilityPool(t)
	if rec := updateConfigForTest(`{"headers":{"X-Remove":"old"}}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := updateConfigForTest(`{"headers":{"X-Keep":"new"},"replaceHeaders":true}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	pool = nil
	loadPool()
	if !reflect.DeepEqual(getProxyConfig().Headers, map[string]string{"X-Keep": "new"}) {
		t.Fatalf("headers not replaced: %v", getProxyConfig().Headers)
	}
}

func TestAnthropicTokenLimitAndMultipleToolResults(t *testing.T) {
	useTemporaryPool(t)
	var req anthropicReq
	if err := json.Unmarshal([]byte(`{"model":"vendor/model","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"one"},{"type":"tool_result","tool_use_id":"call_2","content":"two"},{"type":"text","text":"continue"}]}]}`), &req); err != nil {
		t.Fatal(err)
	}
	body := buildUpstreamBody(anthropicToOpenAI(req), false)
	if body["max_tokens"] != 64 {
		t.Fatalf("token budget=%v", body["max_tokens"])
	}
	var results []string
	userText := false
	for _, v := range body["messages"].([]any) {
		m := v.(map[string]any)
		if m["role"] == "tool" {
			results = append(results, m["tool_call_id"].(string))
		}
		if m["role"] == "user" && m["content"] == "continue" {
			userText = true
		}
	}
	if !reflect.DeepEqual(results, []string{"call_1", "call_2"}) || !userText {
		t.Fatalf("lost tool results or user text: %v", body["messages"])
	}
}

func TestAnthropicNonStreamKeepsToolUseBlocks(t *testing.T) {
	p := seedReliabilityPool(t)
	p.Accounts[0].AccessToken = "workos:cached"
	p.Accounts[0].ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		return reliabilityResponse(200, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"test\"}"}}]},"finish_reason":"tool_calls"}]}`), nil
	})
	rec := httptest.NewRecorder()
	handleAnthropicMessages(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"vendor/model","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)))
	var response struct {
		Content []struct {
			Type string
			ID   string
		}
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "tool_use" || response.Content[0].ID != "call_1" || response.StopReason != "tool_use" {
		t.Fatal(rec.Body.String())
	}
}

func TestRefreshRequestHasDeadline(t *testing.T) {
	mockReliabilityHTTP(t, func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("refresh must have a deadline")
		}
		return nil, context.DeadlineExceeded
	})
	if _, err := refreshClineToken("test"); err == nil {
		t.Fatal("expected timeout")
	}
}
