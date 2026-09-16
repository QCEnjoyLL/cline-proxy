package main

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// accountDetailData 是「账号详情」的响应。
type accountDetailData struct {
	AccountID string `json:"accountId"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	Disabled  bool   `json:"disabled"`
	// CooldownMinutes 是当前配置的兜底冷却时长（分钟）。
	CooldownMinutes int `json:"cooldownMinutes"`
	// Limited 是当前处于「额度用尽 / 冷却」状态的「模型」列表。
	Limited []cooldownEntry `json:"limited"`
	// OtherModels 是配置里可用、且当前未受限的模型，供对照。
	OtherModels []string `json:"otherModels"`
	// 用量计数（代理自己统计的，不是上游权威值）。
	DailyUsageCount int64  `json:"dailyUsageCount"`
	UsageCount      int64  `json:"usageCount"`
	LastUsed        string `json:"lastUsed,omitempty"`
	CreatedAt       string `json:"createdAt,omitempty"`
}

// GET /admin/api/accounts/detail?accountId=xxx
//
// 账号详情：这个账号现在哪些模型到了上限、什么时候重置、还有哪些模型可用。
//
// 为什么需要它：冷却表只按「账号×模型」平铺，看不出「某个账号下哪些模型还能用」；
// 而额度的粒度恰恰就是账号×模型（同账号各模型独立计额），所以视图要对齐这个粒度。
func handleAdminAccountDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}

	accountID := r.URL.Query().Get("accountId")
	if accountID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId is required"})
		return
	}

	acc := getAccountByID(accountID)
	if acc == nil {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "account not found"})
		return
	}

	now := time.Now()

	// 拷贝而不是持有池锁：后面的 allModels()/cooldowns 会各自取锁，
	// 持锁期间调用它们会形成锁嵌套。
	poolMu.Lock()
	email, status, disabled := acc.Email, acc.Status, acc.Disabled
	daily, total, lastUsed, createdAt := acc.DailyUsageCount, acc.UsageCount, acc.LastUsed, acc.CreatedAt
	poolMu.Unlock()

	// 用量计数同样按「本地日」口径展示，与账号列表一致。
	if acc.DailyUsageDate != currentDateKey(now) {
		daily = 0
	}

	limited := cooldowns.entriesFor(accountID, now)
	if limited == nil {
		limited = []cooldownEntry{}
	}

	// 受限的模型不再列入「可用」，避免同一模型在两处出现。
	limitedSet := make(map[string]bool, len(limited))
	for _, e := range limited {
		limitedSet[e.ModelID] = true
	}
	other := make([]string, 0, len(limitedSet))
	for _, m := range allModels() {
		if !limitedSet[m.ID] {
			other = append(other, m.ID)
		}
	}

	data := accountDetailData{
		AccountID:       accountID,
		Email:           email,
		Status:          status,
		Disabled:        disabled,
		CooldownMinutes: cooldownMinutes(),
		Limited:         limited,
		OtherModels:     other,
		DailyUsageCount: daily,
		UsageCount:      total,
	}
	if !lastUsed.IsZero() {
		data.LastUsed = lastUsed.Format(time.RFC3339)
	}
	if !createdAt.IsZero() {
		data.CreatedAt = createdAt.Format(time.RFC3339)
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: data})
}

// POST /admin/api/cooldowns/clear  body: { accountId, modelId }
//
// 手动解除单个「账号 × 模型」的冷却，让请求立即回到轮询。
//
// 用途：冷却时长是我们按 429 猜的（上游有时不说明重置时间），
// 猜错了（比如实际额度已恢复）时用户要能自己纠正，而不是干等。
func handleAdminCooldownClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		AccountID string `json:"accountId"`
		ModelID   string `json:"modelId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	if req.AccountID == "" || req.ModelID == "" {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "accountId and modelId are required"})
		return
	}

	if !cooldowns.clearModel(req.AccountID, req.ModelID) {
		writeAPI(w, http.StatusNotFound, apiResponse{Error: "no active cooldown for this account and model"})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Cooldown cleared"})
}

// 发送到 browser 前把版本占位符替换掉（见 version.go）。
func (d accountDetailData) placeholder() {}