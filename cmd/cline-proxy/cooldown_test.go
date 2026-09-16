package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func resetCooldowns() {
	cooldowns.mu.Lock()
	cooldowns.cooldowns = map[string]time.Time{}
	cooldowns.mu.Unlock()
	setProxyConfig(defaultProxyConfig())
}

// mark/isCooling 基本生命周期：标记后冷却中，TTL 后自动恢复。
func TestCooldownMarkAndExpiry(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	now := time.Now()
	cooldowns.mark("acc1", "vendor/modelA", now)

	if !cooldowns.isCooling("acc1", "vendor/modelA", now.Add(time.Second)) {
		t.Fatal("刚标记的组合应在冷却中")
	}
	// 默认 TTL 30 分钟，超过后应恢复
	after := now.Add(31 * time.Minute)
	if cooldowns.isCooling("acc1", "vendor/modelA", after) {
		t.Fatal("超过 TTL 后应自动恢复")
	}
}

// 同账号不同模型互不影响——这是本次改造的核心目标。
func TestCooldownIsPerModel(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	now := time.Now()
	cooldowns.mark("acc1", "vendor/modelA", now)

	if !cooldowns.isCooling("acc1", "vendor/modelA", now.Add(time.Minute)) {
		t.Fatal("modelA 应在冷却中")
	}
	if cooldowns.isCooling("acc1", "vendor/modelB", now.Add(time.Minute)) {
		t.Fatal("modelB 不应受 modelA 冷却影响")
	}
	if cooldowns.isCooling("acc2", "vendor/modelA", now.Add(time.Minute)) {
		t.Fatal("其它账号不应受影响")
	}
}

// 手动启用账号应清空其全部冷却。
func TestCooldownClearAccount(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	now := time.Now()
	cooldowns.mark("acc1", "a/one", now)
	cooldowns.mark("acc1", "a/two", now)
	cooldowns.mark("acc2", "a/one", now)

	cooldowns.clearAccount("acc1")

	if cooldowns.isCooling("acc1", "a/one", now) || cooldowns.isCooling("acc1", "a/two", now) {
		t.Fatal("启用账号后其冷却应全部清除")
	}
	if !cooldowns.isCooling("acc2", "a/one", now) {
		t.Fatal("其它账号的冷却不应被连带清除")
	}
}

// pickAccount 必须跳过「该模型在冷却中」的账号，但保留同账号给其它模型。
func TestPickAccountSkipsCoolingModel(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	now := time.Now()
	// 池里只有一个账号（defaultModels 的行为不变）
	p := loadPool()
	poolMu.Lock()
	p.Accounts = []*Account{{
		AccountID:    "acc_x",
		Email:        "x@test.com",
		RefreshToken: "rt",
		Status:       "active",
	}}
	poolMu.Unlock()

	// 未冷却：可选中
	if acc := pickAccount("vendor/modelA"); acc == nil {
		t.Fatal("未冷却时应能选中账号")
	}

	// modelA 冷却：modelA 选不到
	cooldowns.mark("acc_x", "vendor/modelA", now)
	if acc := pickAccount("vendor/modelA"); acc != nil {
		t.Fatal("模型冷却中不应选中该账号")
	}
	// 同一账号 modelB 仍可用
	acc := pickAccount("vendor/modelB")
	if acc == nil || acc.AccountID != "acc_x" {
		t.Fatal("其它模型应仍能选中同一账号")
	}
	// modelID 为空时不过滤模型（兼容旧行为）
	if acc := pickAccount(""); acc == nil {
		t.Fatal("不传模型时应忽略模型级冷却")
	}
}

// 手动禁用的账号任何模型都不参与轮询。
func TestPickAccountSkipsDisabled(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	p := loadPool()
	poolMu.Lock()
	p.Accounts = []*Account{{
		AccountID:    "acc_d",
		Email:        "d@test.com",
		RefreshToken: "rt",
		Status:       "active",
		Disabled:     true,
	}}
	poolMu.Unlock()

	if acc := pickAccount("vendor/modelA"); acc != nil {
		t.Fatal("手动禁用的账号不应参与轮询")
	}
}

// 启用/禁用 API：禁用后 pickAccount 跳过，启用后恢复，且启用清空冷却。
func TestEnableDisableAccountAPI(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	p := loadPool()
	poolMu.Lock()
	p.Accounts = []*Account{{
		AccountID:    "acc_api",
		Email:        "api@test.com",
		RefreshToken: "rt",
		Status:       "active",
	}}
	poolMu.Unlock()

	post := func(path, id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		body := `{"accountId":"` + id + `"}`
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if path == "/admin/api/accounts/disable" {
			handleAdminAccountDisable(rec, req)
		} else {
			handleAdminAccountEnable(rec, req)
		}
		return rec
	}

	// 禁用
	if rec := post("/admin/api/accounts/disable", "acc_api"); rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if acc := pickAccount("vendor/m"); acc != nil {
		t.Fatal("禁用后不应被选中")
	}
	// 标记已持久化
	p2 := loadPool()
	if !p2.Accounts[0].Disabled {
		t.Fatal("禁用标记应写入账号池")
	}

	// 重复禁用同一账号：幂等
	if rec := post("/admin/api/accounts/disable", "acc_api"); rec.Code != http.StatusOK {
		t.Fatalf("重复 disable status = %d", rec.Code)
	}

	// 启用（同时清空冷却）
	cooldowns.mark("acc_api", "vendor/m", time.Now())
	if rec := post("/admin/api/accounts/enable", "acc_api"); rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if acc := pickAccount("vendor/m"); acc == nil {
		t.Fatal("启用后应恢复轮询")
	}
	if cooldowns.isCooling("acc_api", "vendor/m", time.Now()) {
		t.Fatal("启用应清空该账号全部冷却")
	}

	// 不存在的账号
	if rec := post("/admin/api/accounts/disable", "acc_nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account status = %d, want 404", rec.Code)
	}
}

// 配置的 cooldownMinutes 生效：改短 TTL 后冷却提前结束。
func TestCooldownTTLFromConfig(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	cfg := getProxyConfig()
	cfg.CooldownMinutes = 1
	setProxyConfig(cfg)

	now := time.Now()
	cooldowns.mark("acc1", "m", now)
	if cooldowns.isCooling("acc1", "m", now.Add(90*time.Second)) {
		t.Fatal("TTL=1 分钟时 90 秒后应已恢复")
	}
}
