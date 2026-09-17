package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeUpstreamFor 起一个假上游：/auth/refresh 给 token，/chat/completions 回指定状态码。
// 返回的 cleanup 负责关闭 server 并还原 clineAPIBase。
func fakeUpstreamFor(t *testing.T, chatStatus int, chatBody map[string]any) func() {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/refresh":
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"accessToken":  "access-token",
				"refreshToken": "refresh-token",
				"expiresAt":    time.Now().Add(24 * time.Hour).UnixMilli(),
			}})
		case "/chat/completions":
			w.WriteHeader(chatStatus)
			if chatBody != nil {
				json.NewEncoder(w).Encode(chatBody)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	originalBase := clineAPIBase
	clineAPIBase = server.URL
	return func() {
		clineAPIBase = originalBase
		server.Close()
	}
}

func addActiveTestAccount(t *testing.T) {
	t.Helper()
	addAccount(&Account{
		AccountID:    "acc_upstream_status",
		Email:        "status@example.com",
		Status:       "active",
		RefreshToken: "refresh-token",
		AccessToken:  "workos:access-token",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	})
}

// 关键回归：callClineAPI 必须把上游状态码带进 error，否则上层只能回 500。
//
// 旧实现返回的是裸 fmt.Errorf("API %d: ...")，状态码只存在于字符串里，
// 调用方无从判断该回什么给客户端——于是全都变成 500。
func TestCallClineAPIErrorCarriesUpstreamStatus(t *testing.T) {
	cases := []struct {
		upstream   int
		wantClient int
		wantType   string
		// wantUpstreamBody 表示这次走「把上游响应体原样带回」的分支。
		// 401 是例外：代码会先刷新 token 再重试，第二次仍 401 就判定账号凭据
		// 永久失效（account_error），不带上游原文——这是既有的正确处理。
		wantUpstreamBody bool
	}{
		{http.StatusTooManyRequests, http.StatusTooManyRequests, "upstream_error", true},
		{http.StatusNotFound, http.StatusNotFound, "upstream_error", true},
		{http.StatusBadRequest, http.StatusBadRequest, "upstream_error", true},
		{http.StatusInternalServerError, http.StatusBadGateway, "upstream_error", true},
		{http.StatusUnauthorized, http.StatusBadGateway, "account_error", false},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.upstream), func(t *testing.T) {
			useTemporaryPool(t)
			resetCooldowns()
			t.Cleanup(resetCooldowns)

			cleanup := fakeUpstreamFor(t, tc.upstream, map[string]any{
				"error": map[string]any{"message": "upstream said no"},
			})
			t.Cleanup(cleanup)
			addActiveTestAccount(t)

			_, err := callClineAPI(context.Background(), map[string]any{
				"model":    "cline-free/glm-5.2",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}, false)
			if err == nil {
				t.Fatalf("上游 %d 应返回错误", tc.upstream)
			}

			var ue *upstreamError
			if !errors.As(err, &ue) {
				t.Fatalf("错误应能解出 *upstreamError（否则上层无法判断状态码），实际类型 %T: %v",
					err, err)
			}
			if ue.Status != tc.wantClient {
				t.Errorf("Status = %d, want %d（上游 %d）", ue.Status, tc.wantClient, tc.upstream)
			}
			if ue.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", ue.Type, tc.wantType)
			}
			// 上游原文要保留在消息里，便于客户端和排查者看到真实原因。
			hasBody := containsStr(ue.Message, "upstream said no")
			if tc.wantUpstreamBody && !hasBody {
				t.Errorf("Message 应包含上游原文，实际 %q", ue.Message)
			}
			if !tc.wantUpstreamBody && hasBody {
				t.Errorf("该分支不该带上游原文，实际 %q", ue.Message)
			}
			// 关键回归点：任何情况下都不该是 500。
			if ue.Status == http.StatusInternalServerError {
				t.Error("不该返回 500（旧实现的问题就是一律 500）")
			}
		})
	}
}

// 冷到没有可用账号时必须回 503（重试有意义），而不是 500。
func TestCallClineAPINoAccountIsServiceUnavailable(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	// 池子里有账号但被手动禁用：pickAccount 会跳过它。
	addAccount(&Account{
		AccountID:    "acc_disabled",
		Email:        "disabled@example.com",
		Status:       "active",
		Disabled:     true,
		RefreshToken: "refresh-token",
		AccessToken:  "workos:access-token",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	})

	_, err := callClineAPI(context.Background(), map[string]any{
		"model":    "cline-free/glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false)
	if err == nil {
		t.Fatal("没有可用账号时应返回错误")
	}

	var ue *upstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("应为 *upstreamError，实际 %T: %v", err, err)
	}
	if ue.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503", ue.Status)
	}
	if ue.Type != "no_account" {
		t.Errorf("Type = %q, want no_account", ue.Type)
	}
}

// 回归：上游首次 401 时，必须真的「刷新 token 并重试」，且成功时不能把账号下线。
//
// 背景：这个分支曾被改坏成「刷新成功后整段重试被跳过」——retryReq 的错误判断把
// 后续代码全包进了 if rerr != nil 里。后果很严重：
//  1. 重试永不发生，本来刷新就能救回的请求必然失败；
//  2. 流程落到后面的 401 判断（resp 仍是原始 401），把**刚刚刷新成功**的账号
//     改写成 Status="expired" 并落盘；
//  3. pickAccount 跳过一切非 active 账号 —— 一次上游 401 就让该账号退出轮询，
//     直到管理员手动恢复。
//
// 所以这条测试同时断言：重试确实发出去了（命中次数 >= 2）、请求最终成功、
// 账号仍是 active。
func TestCallClineAPIRetriesAfterTokenRefreshAndKeepsAccountActive(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	var chatHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/refresh":
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"accessToken":  "fresh-access-token",
				"refreshToken": "refresh-token",
				"expiresAt":    time.Now().Add(24 * time.Hour).UnixMilli(),
			}})
		case "/chat/completions":
			chatHits++
			if chatHits == 1 {
				// 第一次：凭据过期。
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "token expired"}})
				return
			}
			// 重试：这一次必须带上刷新后的 token，并成功。
			if got := r.Header.Get("Authorization"); got != "Bearer workos:fresh-access-token" {
				t.Errorf("重试未使用刷新后的 token，Authorization = %q", got)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-1",
				"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	originalBase := clineAPIBase
	clineAPIBase = server.URL
	t.Cleanup(func() {
		clineAPIBase = originalBase
		server.Close()
	})

	addAccount(&Account{
		AccountID:    "acc_401_retry",
		Email:        "retry@example.com",
		Status:       "active",
		RefreshToken: "refresh-token",
		AccessToken:  "workos:stale-access-token",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	})

	resp, err := callClineAPI(context.Background(), map[string]any{
		"model":    "cline-free/glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false)
	if err != nil {
		t.Fatalf("首次 401 后刷新 token 应能救回请求，实际报错：%v", err)
	}
	resp.Body.Close()

	if chatHits < 2 {
		t.Errorf("上游 /chat/completions 命中 %d 次，说明首次 401 后**没有重试**", chatHits)
	}

	// 账号绝不能被标记为 expired——它是刷新成功的。
	for _, a := range listAccounts() {
		if a.AccountID == "acc_401_retry" && a.Status != "active" {
			t.Errorf("刷新成功后账号状态 = %q，应为 active（否则会退出轮询直到人工恢复）", a.Status)
		}
	}
}

func containsStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
