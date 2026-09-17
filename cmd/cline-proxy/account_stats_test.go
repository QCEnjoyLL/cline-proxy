package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 守卫在「启动时为空池、之后才加账号」的情况下必须放过请求。
//
// 说明：本条测的是**行为没有退化**，不是说旧写法就有这个 bug。旧写法是
// `activeCount == 0 && len(loadPool().Accounts) == 0`，靠 && 短路，账号非空时
// 守卫本来就不会触发。改成 snapshotAccountStats 是为了消除「锁外读 slice
// header」的 data race（见 TestAccountStatsConcurrentWithMutations），
// 本条只保证这次改动没把原来的行为改坏。
func TestHandlersRecoverAfterAccountsAddedPostStartup(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	// 模拟启动时的空池快照：此时 snapshot 应报告 0/0。
	if s := snapshotAccountStats(); s.Total != 0 || s.Active != 0 {
		t.Fatalf("初始应为空池，实际 total=%d active=%d", s.Total, s.Active)
	}

	// 模拟「启动之后」管理员通过面板添加账号。
	addAccount(&Account{
		AccountID:    "acc_late",
		Email:        "late@example.com",
		Status:       "active",
		RefreshToken: "refresh-token",
		AccessToken:  "workos:access-token",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	})

	// 守卫必须看到「已有账号」，不再回 503 no_account。
	s := snapshotAccountStats()
	if s.Total != 1 || s.Active != 1 {
		t.Fatalf("添加账号后应为 total=1 active=1，实际 total=%d active=%d", s.Total, s.Active)
	}
	// 自我检查：确认快照确实反映了新账号（否则下面的端到端断言没有意义）。
	if s.Active == 0 && s.Total == 0 {
		t.Fatal("添加账号后快照仍为 0/0，统计函数有问题")
	}

	// 端到端：Anthropic 端点的守卫应放过请求。用假上游，避免测试打真实网络
	// （真网络会更慢、依赖外网，还可能被上游限流干扰）。
	// 下游具体回什么（502/200）不是关注点——只要不是「空池 503」，就说明守卫
	// 看到了新加的账号。
	cleanup := fakeUpstreamFor(t, http.StatusInternalServerError, map[string]any{
		"error": map[string]any{"message": "irrelevant"},
	})
	t.Cleanup(cleanup)

	rec := httptest.NewRecorder()
	body := `{"model":"cline-free/glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	handleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), "no_account") {
		t.Errorf("已加账号却仍回空池 503：%s", rec.Body.String())
	}
}

// snapshotAccountStats / accountCount 必须在锁内读，且与真实池一致。
func TestAccountStatsHelpersAgreeWithPool(t *testing.T) {
	useTemporaryPool(t)

	if got := accountCount(); got != 0 {
		t.Errorf("空池 accountCount() = %d, want 0", got)
	}

	addAccount(&Account{AccountID: "a1", Email: "a1@e.com", Status: "active"})
	addAccount(&Account{AccountID: "a2", Email: "a2@e.com", Status: "active", Disabled: true})
	addAccount(&Account{AccountID: "a3", Email: "a3@e.com", Status: "expired"})

	if got := accountCount(); got != 3 {
		t.Errorf("accountCount() = %d, want 3", got)
	}
	s := snapshotAccountStats()
	if s.Total != 3 {
		t.Errorf("Total = %d, want 3", s.Total)
	}
	// Active 只统计 Status == "active"，**不**扣除 Disabled——这与
	// snapshotAccountStats 的定位一致：它是给「池子里有没有可用账号」这类守卫用的
	// 粗判，不是给 admin 面板那套「活跃/已禁用/已过期」三分类用的
	// （面板用的是 handleAdminStats 里更细的统计）。
	// a2 虽然被手动禁用，但 Status 仍是 "active"，所以这里算 2。
	if s.Active != 2 {
		t.Errorf("Active = %d, want 2（a1 与 a2 的 Status 都是 active）", s.Active)
	}
}

// 并发下反复取统计不该触发 data race（配合 -race 运行更有意义；
// 无 -race 时至少能覆盖 panic / 逻辑错乱）。
func TestAccountStatsConcurrentWithMutations(t *testing.T) {
	useTemporaryPool(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			addAccount(&Account{
				AccountID: "acc_concurrent",
				Email:     "c@e.com",
				Status:    "active",
			})
		}
	}()

	for i := 0; i < 200; i++ {
		_ = snapshotAccountStats()
		_ = accountCount()
	}
	<-done

	if got := accountCount(); got != 200 {
		t.Errorf("accountCount() = %d, want 200", got)
	}
}
