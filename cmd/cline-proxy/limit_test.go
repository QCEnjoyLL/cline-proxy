package main

import (
	"encoding/json"
	"strings"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 免费模型额度用尽：识别种类，并把重置时刻定为次日零点。
//
// 上游文案来自官方客户端源码：CLINE_FREE_MODEL_LIMIT_MARKER =
// "free limit reached on model"。
func TestParseLimitInfoFreeModelDaily(t *testing.T) {
	now := time.Date(2026, 9, 16, 20, 47, 0, 0, time.Local)
	body := `{"error":{"message":"Daily free model limit reached: free limit reached on model cline-free/deepseek-v4.1-flash"}}`

	info := parseLimitInfo(body, now, true)

	if info.Kind != limitKindFreeDaily {
		t.Fatalf("Kind = %q, want %q", info.Kind, limitKindFreeDaily)
	}
	want := time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)
	if !info.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %v, want %v (次日零点)", info.ResetAt, want)
	}
	if info.Detail == "" {
		t.Fatal("应保留上游原文供人工核对")
	}
}

// 上游若直接给出 "try again in X"，优先用它而不是次日零点。
func TestParseLimitInfoUsesUpstreamRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 16, 20, 0, 0, 0, time.Local)
	body := "Daily free model limit reached: free limit reached on model foo/bar. Try again in 45 minutes."

	info := parseLimitInfo(body, now, true)

	if info.Kind != limitKindFreeDaily {
		t.Fatalf("Kind = %q", info.Kind)
	}
	want := now.Add(45 * time.Minute)
	if !info.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %v, want %v（应采纳上游给的时长）", info.ResetAt, want)
	}
}

// 多单位组合："try again in 2 hours 30 minutes"。
func TestParseLimitInfoMultiUnitRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 16, 20, 0, 0, 0, time.Local)
	body := "free limit reached on model a/b. try again in 2 hours 30 minutes"

	info := parseLimitInfo(body, now, true)

	want := now.Add(2*time.Hour + 30*time.Minute)
	if !info.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %v, want %v", info.ResetAt, want)
	}
}

// ClinePass 订阅额度：识别种类；上游不说重置时间，则 ResetAt 为零值。
func TestParseLimitInfoPassLimit(t *testing.T) {
	now := time.Now()
	body := "You have reached your ClinePass limit for this period. Please try again later."

	info := parseLimitInfo(body, now, true)

	if info.Kind != limitKindPassLimit {
		t.Fatalf("Kind = %q, want %q", info.Kind, limitKindPassLimit)
	}
	if !info.ResetAt.IsZero() {
		t.Fatalf("上游没给重置时间时应为零值，实际 %v", info.ResetAt)
	}
}

// 花费上限：从 JSON 里取 resets_at。
func TestParseLimitInfoSpendLimitResetsAt(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	body := `{"error":{"code":"SPEND_LIMIT_EXCEEDED","limit_scope":"user","budget_period":"daily",` +
		`"limit_usd":20.0,"spent_usd":20.5,"resets_at":"2026-09-16T20:00:00.000Z","message":"limit"}}`

	info := parseLimitInfo(body, now, false)

	if info.Kind != limitKindSpendLimit {
		t.Fatalf("Kind = %q, want %q", info.Kind, limitKindSpendLimit)
	}
	want := time.Date(2026, 9, 16, 20, 0, 0, 0, time.UTC)
	if !info.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %v, want %v", info.ResetAt, want)
	}
}

// 无法识别的 429 必须诚实标为 unknown，不能冒充「额度用尽」。
func TestParseLimitInfoUnknown(t *testing.T) {
	now := time.Now()
	for _, body := range []string{"", "rate limited, slow down", `{"error":"something else"}`} {
		info := parseLimitInfo(body, now, true)
		if info.Kind != limitKindUnknown {
			t.Errorf("body=%q 时 Kind = %q, want %q", body, info.Kind, limitKindUnknown)
		}
		if !info.ResetAt.IsZero() {
			t.Errorf("body=%q 时不该有重置时间", body)
		}
	}
}

// 上游给出的荒谬重置时间必须被丢弃（否则某个组合会被锁死很久）。
func TestParseLimitInfoRejectsAbsurdResetTime(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	// 超过 24 小时
	body := `{"error":{"code":"SPEND_LIMIT_EXCEEDED","resets_at":"2027-01-01T00:00:00Z"}}`
	if info := parseLimitInfo(body, now, false); !info.ResetAt.IsZero() {
		t.Errorf("超过 24 小时的重置时间应被丢弃，实际 %v", info.ResetAt)
	}

	// 已过去的时间
	body2 := `{"error":{"code":"SPEND_LIMIT_EXCEEDED","resets_at":"2026-09-15T00:00:00Z"}}`
	if info := parseLimitInfo(body2, now, false); !info.ResetAt.IsZero() {
		t.Errorf("已过去的重置时间应被丢弃，实际 %v", info.ResetAt)
	}
}

// 上游给了重置时间时，冷却时长应比配置的固定时长更长
// （否则会在真实重置前把请求放回去，立刻再撞 429）。
func TestMarkCooldownPrefersUpstreamResetTime(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	// 配置 1 分钟兜底
	if err := setCooldownMinutes(1); err != nil {
		t.Fatalf("setCooldownMinutes: %v", err)
	}

	acc := &Account{AccountID: "acc_r", Email: "r@test.com"}
	reset := time.Now().Add(5 * time.Hour)
	ttl := markCooldown(acc, "cline-free/x", limitInfo{
		Kind:    limitKindFreeDaily,
		ResetAt: reset,
	})

	if ttl < 4*time.Hour+50*time.Minute {
		t.Fatalf("TTL = %v，应采纳上游重置时间（约 5 小时）而不是配置的 1 分钟", ttl)
	}
	if !cooldowns.isCooling("acc_r", "cline-free/x", time.Now()) {
		t.Fatal("应按标记进入冷却")
	}
	if cooldowns.isCooling("acc_r", "cline-free/x", reset.Add(time.Minute)) {
		t.Fatal("到重置时间后应自动恢复")
	}
}

// 详情接口：必须给出「哪些模型受限 + 何时重置」以及邮箱。
func TestAccountDetailReportsLimitedModels(t *testing.T) {
	useTemporaryPool(t)
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	p := loadPool()
	poolMu.Lock()
	p.Accounts = []*Account{{
		AccountID: "acc_d", Email: "detail@test.com", Status: "active",
	}}
	poolMu.Unlock()

	reset := time.Now().Add(3 * time.Hour)
	cooldowns.mark("acc_d", "detail@test.com", "cline-free/glm-5.2", time.Now(), time.Hour, limitInfo{
		Kind:    limitKindFreeDaily,
		Detail:  "free limit reached on model cline-free/glm-5.2",
		ResetAt: reset,
	})

	rec := httptest.NewRecorder()
	handleAdminAccountDetail(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/detail?accountId=acc_d", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Email     string `json:"email"`
			AccountID string `json:"accountId"`
			Limited   []struct {
				Email        string `json:"email"`
				ModelID      string `json:"modelId"`
				Kind         string `json:"kind"`
				Limited      bool   `json:"limited"`
				ResetsAt     int64  `json:"resetsAt"`
				RemainingSec int64  `json:"remainingSec"`
			} `json:"limited"`
			OtherModels []string `json:"otherModels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}

	if !resp.Success || resp.Data.Email != "detail@test.com" || resp.Data.AccountID != "acc_d" {
		t.Fatalf("基本字段不对：%s", rec.Body.String())
	}
	if len(resp.Data.Limited) != 1 {
		t.Fatalf("limited 应有 1 条，实际 %d：%s", len(resp.Data.Limited), rec.Body.String())
	}
	l := resp.Data.Limited[0]
	if l.ModelID != "cline-free/glm-5.2" || l.Kind != string(limitKindFreeDaily) || !l.Limited {
		t.Fatalf("受限模型字段不对：%+v", l)
	}
	if l.Email != "detail@test.com" {
		t.Fatalf("受限条目应带邮箱（前端用它显示身份）：%+v", l)
	}
	if l.ResetsAt <= time.Now().UnixMilli() {
		t.Fatalf("resetsAt 应指向未来：%d", l.ResetsAt)
	}
	// 受限的模型不应同时出现在「可用模型」里
	for _, m := range resp.Data.OtherModels {
		if m == "cline-free/glm-5.2" {
			t.Fatal("受限模型不应出现在 otherModels 里")
		}
	}
}

func TestAccountDetailUnknownAccount(t *testing.T) {
	useTemporaryPool(t)

	rec := httptest.NewRecorder()
	handleAdminAccountDetail(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/detail?accountId=nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAccountDetailRequiresAccountID(t *testing.T) {
	rec := httptest.NewRecorder()
	handleAdminAccountDetail(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/detail", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// 手动解除单个组合的冷却。
func TestCooldownClearSingleModel(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	cooldowns.mark("acc_c", "c@test.com", "a/one", time.Now(), time.Hour, limitInfo{})
	cooldowns.mark("acc_c", "c@test.com", "a/two", time.Now(), time.Hour, limitInfo{})

	body := `{"accountId":"acc_c","modelId":"a/one"}`
	rec := httptest.NewRecorder()
	handleAdminCooldownClear(rec, httptest.NewRequest(http.MethodPost, "/admin/api/cooldowns/clear", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if cooldowns.isCooling("acc_c", "a/one", time.Now()) {
		t.Fatal("a/one 的冷却应已解除")
	}
	if !cooldowns.isCooling("acc_c", "a/two", time.Now()) {
		t.Fatal("同账号其它模型的冷却不应被连带解除")
	}

	// 再解一次：已无冷却 → 404
	rec2 := httptest.NewRecorder()
	handleAdminCooldownClear(rec2, httptest.NewRequest(http.MethodPost, "/admin/api/cooldowns/clear", strings.NewReader(body)))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("重复解除 status = %d, want 404", rec2.Code)
	}
}

func TestCooldownClearValidatesInput(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	cases := []string{`{}`, `{"accountId":"a"}`, `{"modelId":"m"}`, `not json`}
	for _, body := range cases {
		rec := httptest.NewRecorder()
		handleAdminCooldownClear(rec, httptest.NewRequest(http.MethodPost, "/admin/api/cooldowns/clear", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%q 时 status = %d, want 400", body, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	handleAdminCooldownClear(rec, httptest.NewRequest(http.MethodGet, "/admin/api/cooldowns/clear", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}

// /cooldowns 的条目必须带邮箱，前端才能显示邮箱而不是账号 ID。
func TestCooldownsSnapshotCarriesEmail(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	cooldowns.mark("acc_m", "who@test.com", "vendor/x", time.Now(), time.Hour, limitInfo{
		Kind:    limitKindFreeDaily,
		ResetAt: time.Now().Add(30 * time.Minute),
	})

	got := cooldowns.snapshot(time.Now())
	if len(got) != 1 {
		t.Fatalf("snapshot 应返回 1 条，实际 %d", len(got))
	}
	if got[0].Email != "who@test.com" {
		t.Fatalf("Email = %q, want who@test.com", got[0].Email)
	}
	if !got[0].Limited || got[0].Kind != string(limitKindFreeDaily) {
		t.Fatalf("应标记为「额度用尽」：%+v", got[0])
	}
	if got[0].ResetsAtUnixMs == 0 {
		t.Fatal("应带上游给出的重置时间")
	}
}

// 快照按恢复时间升序：最先恢复的排最前。
func TestCooldownSnapshotSortedByReset(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	now := time.Now()
	cooldowns.mark("a3", "e@test.com", "m", now, 3*time.Hour, limitInfo{})
	cooldowns.mark("a1", "e@test.com", "m", now, time.Hour, limitInfo{})
	cooldowns.mark("a2", "e@test.com", "m", now, 2*time.Hour, limitInfo{})

	got := cooldowns.snapshot(now)
	if len(got) != 3 {
		t.Fatalf("应返回 3 条，实际 %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].UntilUnixMs < got[i-1].UntilUnixMs {
			t.Fatalf("未按恢复时间升序：%d 在 %d 之后", got[i].UntilUnixMs, got[i-1].UntilUnixMs)
		}
	}
	if got[0].AccountID != "a1" {
		t.Fatalf("最先恢复的应是 a1，实际 %q", got[0].AccountID)
	}
}

// map 上限保护：不能因为持续标记而无限增长。
func TestCooldownStoreBoundedGrowth(t *testing.T) {
	resetCooldowns()
	t.Cleanup(resetCooldowns)

	// 用「过去」标记记录，这样它们在 now 时刻已经全部过期。
	past := time.Now().Add(-time.Hour)
	for i := 0; i < maxCooldownRecords; i++ {
		cooldowns.mark(accountIDN(i), "e@test.com", "m", past, time.Minute, limitInfo{})
	}

	// 已满且在 now 时刻全部过期 → mark 应先清理再写入
	now := time.Now()
	cooldowns.mark("fresh", "e@test.com", "m", now, time.Hour, limitInfo{})

	cooldowns.mu.Lock()
	size := len(cooldowns.records)
	cooldowns.mu.Unlock()

	if size > maxCooldownRecords {
		t.Fatalf("记录数 %d 超过上限 %d", size, maxCooldownRecords)
	}
	if !cooldowns.isCooling("fresh", "m", now) {
		t.Fatal("清理过期记录后应能写入新记录")
	}
}

func accountIDN(i int) string {
	return "acc_" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
}
