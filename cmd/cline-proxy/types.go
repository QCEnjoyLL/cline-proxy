package main

import "time"

type Account struct {
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
}

type AccountPool struct {
	Accounts       []*Account `json:"accounts"`
	CurrentIdx     int        `json:"currentIdx"`
	Keys           []string   `json:"keys,omitempty"`
	CustomModels   []string   `json:"customModels,omitempty"`
	DisabledModels []string   `json:"disabledModels,omitempty"`
	DefaultModel   string     `json:"defaultModel,omitempty"`
}

