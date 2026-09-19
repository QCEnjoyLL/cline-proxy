package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func resetCooldowns() {
	cooldowns.mu.Lock()
	cooldowns.records = map[string]cooldownRecord{}
	cooldowns.mu.Unlock()
	setProxyConfig(defaultProxyConfig())
}

// mark/isCooling 基本生命周期：标记后冷却中，TTL 后自动恢复。
func TestCooldownMarkAndExpiry(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	now := time.Now()
	// 用真实默认值而非写死数字，避免默认值调整后测试失效
	ttl := time.Duration(defaultCooldownMinutes) * time.Minute
	cooldowns.mark("acc1", "acc1@test.com", "vendor/modelA", now, ttl, limitInfo{})

	if !cooldowns.isCooling("acc1", "vendor/modelA", now.Add(time.Second)) {
		t.Fatal("刚标记的组合应在冷却中")
	}
	// 默认 TTL 30 分钟，超过后应自动恢复
	after := now.Add(ttl + time.Minute)
	if cooldowns.isCooling("acc1", "vendor/modelA", after) {
		t.Fatal("超过 TTL 后应自动恢复")
	}
}

// 同账号不同模型互不影响——这是本次改造的核心目标。
func TestCooldownIsPerModel(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	now := time.Now()
	cooldowns.mark("acc1", "acc1@test.com", "vendor/modelA", now, time.Hour, limitInfo{})

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
	cooldowns.mark("acc1", "acc1@test.com", "a/one", now, time.Hour, limitInfo{})
	cooldowns.mark("acc1", "acc1@test.com", "a/two", now, time.Hour, limitInfo{})
	cooldowns.mark("acc2", "acc2@test.com", "a/one", now, time.Hour, limitInfo{})

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
	// 池里只有一个账号。
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
	cooldowns.mark("acc_x", "acc_x@test.com", "vendor/modelA", now, time.Hour, limitInfo{})
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
	// 标记已落盘：先丢弃内存缓存再重载（loadPool 命中缓存会返回同一个对象，
	// 不置 nil 的话断言读的是内存而不是文件，恒为真）
	pool = nil
	if p2 := loadPool(); len(p2.Accounts) != 1 || !p2.Accounts[0].Disabled {
		t.Fatal("禁用标记应写入账号池文件")
	}

	// 重复禁用同一账号：幂等
	if rec := post("/admin/api/accounts/disable", "acc_api"); rec.Code != http.StatusOK {
		t.Fatalf("重复 disable status = %d", rec.Code)
	}

	// 启用（同时清空冷却）
	cooldowns.mark("acc_api", "acc_api@test.com", "vendor/m", time.Now(), time.Hour, limitInfo{})
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

// cooldownMinutes 存于账号池并落盘；冷却 TTL 以该配置为准。
func TestCooldownMinutesPersistAndApply(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	// 未配置时使用默认值
	if got := cooldownMinutes(); got != defaultCooldownMinutes {
		t.Fatalf("默认冷却时长 = %d, want %d", got, defaultCooldownMinutes)
	}

	if err := setCooldownMinutes(1); err != nil {
		t.Fatalf("setCooldownMinutes: %v", err)
	}
	// 丢弃内存缓存强制从磁盘重载，验证写盘真的生效
	pool = nil
	if got := cooldownMinutes(); got != 1 {
		t.Fatalf("重载后冷却时长 = %d, want 1", got)
	}

	// markCooldown 使用配置的时长：TTL=1 分钟 → 90 秒后应已恢复
	markCooldown(&Account{AccountID: "acc1", Email: "acc1@test.com"}, "m", limitInfo{})
	if !cooldowns.isCooling("acc1", "m", time.Now()) {
		t.Fatal("刚标记的组合应在冷却中")
	}
	if cooldowns.isCooling("acc1", "m", time.Now().Add(90*time.Second)) {
		t.Fatal("TTL=1 分钟时 90 秒后应已恢复")
	}
}

// 回归测试：effectiveModel 必须与实际发给上游的模型一致。
//
// 背景：冷却键由 pickAccount/cooldowns.mark 计算，请求体由 buildUpstreamBody 生成。
// 若两处对「未指定 model」的处理不一致，客户端不传 model 时会标记到
// 一个永远查不到的 key，冷却静默失效。
func TestEffectiveModelMatchesUpstreamBody(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)
	if _, err := addCustomModel("vendor/default"); err != nil {
		t.Fatal(err)
	}

	// 客户端指定模型：完全采用客户端的值
	params := map[string]any{"model": "vendor/explicit", "messages": []any{}}
	if got := effectiveModel(params); got != "vendor/explicit" {
		t.Fatalf("指定模型时 effectiveModel = %q", got)
	}
	if body := buildUpstreamBody(params, false); body["model"] != "vendor/explicit" {
		t.Fatalf("请求体模型 = %v", body["model"])
	}

	// 客户端未指定模型（缺字段 / 空串）：都应回退到默认模型，且两处保持一致
	for _, p := range []map[string]any{
		{"messages": []any{}},
		{"model": "", "messages": []any{}},
	} {
		want := getDefaultModel()
		got := effectiveModel(p)
		if got != want {
			t.Fatalf("未指定模型时应回退到默认模型 %q，实际 %q", want, got)
		}
		if b := buildUpstreamBody(p, false); b["model"] != got {
			t.Fatalf("请求体模型 %v 与冷却键模型 %q 不一致", b["model"], got)
		}
		// 两处一致才意味着：用 effectiveModel 标记的冷却能被同 key 查到
		cooldowns.mark("acc_k", "acc_k@test.com", got, time.Now(), time.Hour, limitInfo{})
		if !cooldowns.isCooling("acc_k", got, time.Now()) {
			t.Fatal("用 effectiveModel 标记的冷却应能被同 key 查询")
		}
		cooldowns.clearAccount("acc_k")
	}
}

// 契约测试：/admin/api/cooldowns 的字段名必须与 web/admin.html 读取的一致
// （accountId / modelId / until / remainingSec）；改了字段名前端会静默显示空表。
func TestCooldownsEndpointShape(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	cooldowns.mark("acc_s", "acc_s@test.com", "vendor/modelA", time.Now(), time.Hour, limitInfo{})

	rec := httptest.NewRecorder()
	handleAdminCooldowns(rec, httptest.NewRequest(http.MethodGet, "/admin/api/cooldowns", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Cooldowns []struct {
				AccountID    string `json:"accountId"`
				ModelID      string `json:"modelId"`
				Until        int64  `json:"until"`
				RemainingSec int64  `json:"remainingSec"`
			} `json:"cooldowns"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if !resp.Success || len(resp.Data.Cooldowns) != 1 {
		t.Fatalf("响应不符合预期（success=%v, n=%d）：%s", resp.Success, len(resp.Data.Cooldowns), rec.Body.String())
	}
	c := resp.Data.Cooldowns[0]
	if c.AccountID != "acc_s" || c.ModelID != "vendor/modelA" {
		t.Fatalf("字段不匹配：%+v", c)
	}
	if c.RemainingSec <= 0 || c.RemainingSec > 3600 {
		t.Fatalf("remainingSec = %d", c.RemainingSec)
	}
	if c.Until <= time.Now().UnixMilli() {
		t.Fatalf("until 应指向未来：%d", c.Until)
	}
}

// 该端点只读：非 GET 必须拒绝，避免误用写方法。
func TestCooldownsEndpointRejectsPost(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	rec := httptest.NewRecorder()
	handleAdminCooldowns(rec, httptest.NewRequest(http.MethodPost, "/admin/api/cooldowns", strings.NewReader("{}")))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
