package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 端到端（走真实代码路径）：上游返回「免费额度用尽」的 429 时，
// 必须记录到「账号 × 模型」并保存上游给出的重置时间。
//
// 这条测试覆盖 callClineAPI 里的 429 分支——只有它同时串起
// 「解析响应体 → 决定冷却时长 → 写入冷却表」三件事，
// 单测单个函数无法发现它们之间的接线错误。
func TestCallClineAPIRecordsFreeModelLimitFromUpstream(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	// 假上游：先发 token，再对 chat/completions 回 429。
	var gotChatBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/auth/refresh":
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"accessToken":  "access-token",
				"refreshToken": "refresh-token",
				"expiresAt":    time.Now().Add(24 * time.Hour).UnixMilli(),
			}})
		case r.URL.Path == "/chat/completions":
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			gotChatBody = string(buf[:n])
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"message": "Daily free model limit reached: free limit reached on model cline-free/deepseek-v4.1-flash. Try again in 45 minutes.",
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	originalBase := clineAPIBase
	clineAPIBase = server.URL
	t.Cleanup(func() { clineAPIBase = originalBase })

	addAccount(&Account{
		AccountID:    "acc_limit",
		Email:        "limit@example.com",
		Status:       "active",
		RefreshToken: "refresh-token",
		AccessToken:  "workos:access-token",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	})

	params := map[string]any{
		"model":    "cline-free/deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	start := time.Now()
	if _, err := callClineAPI(params, false); err == nil {
		t.Fatal("上游 429 应返回错误")
	}

	// 请求体确实发到了上游（确认走的是真实路径）
	if gotChatBody == "" {
		t.Fatal("没有请求到达假上游的 chat/completions")
	}

	var entry *cooldownEntry
	for _, e := range cooldowns.snapshot(time.Now()) {
		if e.AccountID == "acc_limit" {
			entry = &e
			break
		}
	}
	if entry == nil {
		t.Fatal("429 之后应记录「账号 × 模型」冷却")
	}
	if entry.ModelID != "cline-free/deepseek-v4.1-flash" {
		t.Fatalf("模型 = %q", entry.ModelID)
	}
	if entry.Email != "limit@example.com" {
		t.Fatalf("应记录邮箱（界面用它显示身份）：%q", entry.Email)
	}
	if !entry.Limited || entry.Kind != string(limitKindFreeDaily) {
		t.Fatalf("应识别为免费模型额度用尽：kind=%q limited=%v", entry.Kind, entry.Limited)
	}
	if entry.ResetsAtUnixMs == 0 {
		t.Fatal("应保存上游给出的重置时间")
	}

	// 上游说 "try again in 45 minutes"：冷却时长应约为 45 分钟，
	// 而不是设置页里配置的默认 30 分钟。
	remaining := time.Duration(entry.RemainingSec) * time.Second
	if remaining < 40*time.Minute || remaining > 46*time.Minute {
		t.Fatalf("冷却时长 = %v，应采纳上游的 45 分钟（而非配置的 %d 分钟），发起于 %v",
			remaining, defaultCooldownMinutes, start)
	}
}

// 无法识别的 429 仍要冷却（避免立刻重撞），但必须标成「非额度用尽」，
// 且按配置时长，不能凭空编出一个重置时间。
func TestCallClineAPIUnrecognized429UsesConfiguredTTL(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	if err := setCooldownMinutes(3); err != nil {
		t.Fatalf("setCooldownMinutes: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth/refresh" {
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"accessToken": "a", "refreshToken": "r", "expiresAt": time.Now().Add(time.Hour).UnixMilli(),
			}})
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{"error": "slow down"})
	}))
	defer server.Close()

	originalBase := clineAPIBase
	clineAPIBase = server.URL
	t.Cleanup(func() { clineAPIBase = originalBase })

	addAccount(&Account{
		AccountID: "acc_u", Email: "u@example.com", Status: "active",
		RefreshToken: "rt", AccessToken: "workos:a", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})

	if _, err := callClineAPI(map[string]any{"model": "vendor/m", "messages": []any{}}, false); err == nil {
		t.Fatal("应返回错误")
	}

	snap := cooldowns.snapshot(time.Now())
	if len(snap) != 1 {
		t.Fatalf("应有 1 条冷却，实际 %d", len(snap))
	}
	e := snap[0]
	if e.Limited {
		t.Fatal("无法识别的 429 不应标成「额度用尽」")
	}
	if e.ResetsAtUnixMs != 0 {
		t.Fatalf("不应编造重置时间：%d", e.ResetsAtUnixMs)
	}
	remaining := time.Duration(e.RemainingSec) * time.Second
	if remaining < 2*time.Minute+50*time.Second || remaining > 3*time.Minute+5*time.Second {
		t.Fatalf("应按配置的 3 分钟冷却，实际 %v", remaining)
	}
}
