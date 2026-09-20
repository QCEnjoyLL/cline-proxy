package main

import "time"

type Account struct {
	// Protected by poolMu; refresh results are shared by concurrent callers.
	refresh          *accountRefresh
	tokenSavePending bool

	AccountID       string    `json:"accountId"`
	Email           string    `json:"email"`
	RefreshToken    string    `json:"refreshToken"`
	AccessToken     string    `json:"-"`
	ExpiresAt       int64     `json:"-"`
	Status          string    `json:"status"` // active, cooldown, expired
	LastUsed        time.Time `json:"lastUsed"`
	UsageCount      int64     `json:"usageCount"`
	DailyUsageCount int64     `json:"dailyUsageCount"`
	DailyUsageDate  string    `json:"dailyUsageDate,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	// Disabled 是用户手动禁用标记，与 Status 的自动冷却互不相干：
	// 禁用不自动恢复，冷却按 CooldownMinutes 到点自动回池。
	Disabled bool `json:"disabled,omitempty"`
}

type AccountPool struct {
	Accounts     []*Account `json:"accounts"`
	CurrentIdx   int        `json:"currentIdx"`
	Keys         []string   `json:"keys,omitempty"`
	CustomModels []string   `json:"customModels,omitempty"`
	DefaultModel string     `json:"defaultModel,omitempty"`
	// ProxyConfig 保存轮换策略和自定义请求头；旧文件缺省时使用内置配置。
	ProxyConfig *proxyConfigData `json:"proxyConfig,omitempty"`
	// CooldownMinutes 是「账号×模型」级 429 冷却的自动恢复时长（分钟）。
	// 存在账号池文件里以便跨重启保留；0 表示用默认值。
	CooldownMinutes int `json:"cooldownMinutes,omitempty"`
	// PerModel 是「按模型配置上游渠道」的持久化存储，key 是模型 ID。
	// 为空时（未配置）代理行为与之前完全一致，因此不需要旧文件迁移。
	// 见 upstream.go 的 ModelUpstream。
	PerModel map[string]ModelUpstream `json:"perModel,omitempty"`
}
