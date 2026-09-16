package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBumpAccountUsageKeepsTotalAndResetsDailyOnDateChange(t *testing.T) {
	useTemporaryPool(t)

	acc := &Account{}
	day1 := time.Date(2026, 8, 10, 23, 59, 0, 0, time.Local)

	poolMu.Lock()
	bumpAccountUsage(acc, day1)
	bumpAccountUsage(acc, day1)
	bumpAccountUsage(acc, day1)
	poolMu.Unlock()

	if acc.UsageCount != 3 {
		t.Fatalf("total usage = %d, want 3", acc.UsageCount)
	}
	if acc.DailyUsageCount != 3 {
		t.Fatalf("daily usage = %d, want 3", acc.DailyUsageCount)
	}
	if got := currentDateKey(day1); acc.DailyUsageDate != got {
		t.Fatalf("daily usage date = %q, want %q", acc.DailyUsageDate, got)
	}

	// Next calendar day: the daily counter rolls over, total keeps growing.
	day2 := time.Date(2026, 8, 11, 10, 0, 0, 0, time.Local)
	poolMu.Lock()
	bumpAccountUsage(acc, day2)
	poolMu.Unlock()

	if acc.UsageCount != 4 {
		t.Fatalf("total usage after rollover = %d, want 4", acc.UsageCount)
	}
	if acc.DailyUsageCount != 1 {
		t.Fatalf("daily usage after rollover = %d, want 1", acc.DailyUsageCount)
	}
	if got := currentDateKey(day2); acc.DailyUsageDate != got {
		t.Fatalf("daily usage date after rollover = %q, want %q", acc.DailyUsageDate, got)
	}

	// A later increment on the same day stays on the new day's counter.
	poolMu.Lock()
	bumpAccountUsage(acc, time.Date(2026, 8, 11, 23, 0, 0, 0, time.Local))
	poolMu.Unlock()

	if acc.UsageCount != 5 {
		t.Fatalf("total usage = %d, want 5", acc.UsageCount)
	}
	if acc.DailyUsageCount != 2 {
		t.Fatalf("daily usage = %d, want 2", acc.DailyUsageCount)
	}
}

func TestBumpAccountUsagePersistsThroughReload(t *testing.T) {
	useTemporaryPool(t)

	acc := &Account{
		AccountID: "acc_test",
		Email:     "test@example.com",
		Status:    "active",
	}
	poolMu.Lock()
	bumpAccountUsage(acc, time.Now())
	poolMu.Unlock()

	addAccount(acc)
	pool = nil // force reload from disk

	reloaded := getAccountByID("acc_test")
	if reloaded == nil {
		t.Fatal("account not found after reload")
	}
	if reloaded.UsageCount != 1 || reloaded.DailyUsageCount != 1 {
		t.Fatalf("persisted usage total=%d daily=%d, want 1/1",
			reloaded.UsageCount, reloaded.DailyUsageCount)
	}
	if reloaded.DailyUsageDate != currentDateKey(time.Now()) {
		t.Fatalf("persisted daily date = %q, want %q",
			reloaded.DailyUsageDate, currentDateKey(time.Now()))
	}
}

func TestAdminAccountsReportsZeroForStaleDailyUsage(t *testing.T) {
	useTemporaryPool(t)

	lastUsed := time.Now().AddDate(0, 0, -2)
	acc := &Account{
		AccountID:       "acc_stale",
		Email:           "stale@example.com",
		Status:          "active",
		LastUsed:        lastUsed,
		UsageCount:      44,
		DailyUsageCount: 2,
		DailyUsageDate:  currentDateKey(lastUsed),
	}
	addAccount(acc)

	response := httptest.NewRecorder()
	handleAdminAccounts(response, httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("accounts status = %d, body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Data struct {
			Accounts []*Account `json:"accounts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode accounts response: %v", err)
	}
	if len(body.Data.Accounts) != 1 {
		t.Fatalf("listed accounts = %d, want 1", len(body.Data.Accounts))
	}
	listed := body.Data.Accounts[0]
	if listed.DailyUsageCount != 0 {
		t.Fatalf("listed stale daily usage = %d, want 0", listed.DailyUsageCount)
	}
	if listed.UsageCount != 44 {
		t.Fatalf("listed total usage = %d, want 44", listed.UsageCount)
	}
	if !listed.LastUsed.Equal(lastUsed) {
		t.Fatalf("listed last used = %v, want %v", listed.LastUsed, lastUsed)
	}
	if acc.DailyUsageCount != 2 {
		t.Fatalf("listing mutated stored daily usage = %d, want 2", acc.DailyUsageCount)
	}
}

func TestListAccountsKeepsCurrentDailyUsage(t *testing.T) {
	useTemporaryPool(t)

	acc := &Account{
		AccountID:       "acc_current",
		DailyUsageCount: 3,
		DailyUsageDate:  currentDateKey(time.Now()),
	}
	addAccount(acc)

	accounts := listAccounts()
	if got := accounts[0].DailyUsageCount; got != 3 {
		t.Fatalf("listed current daily usage = %d, want 3", got)
	}
}

func TestManualResetClearsBothUsageCounters(t *testing.T) {
	useTemporaryPool(t)

	// Mock the token-refresh upstream so the handler works without network.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"accessToken":  "access-token",
				"refreshToken": "refresh-token",
				"expiresAt":    time.Now().Add(24 * time.Hour).UnixMilli(),
			},
		})
	}))
	defer server.Close()
	originalBase := clineAPIBase
	clineAPIBase = server.URL
	defer func() { clineAPIBase = originalBase }()

	acc := &Account{
		AccountID:       "acc_reset",
		Email:           "reset@example.com",
		Status:          "active",
		UsageCount:      100,
		DailyUsageCount: 12,
		DailyUsageDate:  currentDateKey(time.Now()),
		RefreshToken:    "refresh-token",
	}
	addAccount(acc)

	response := httptest.NewRecorder()
	handleAdminAccountReset(response, httptest.NewRequest(http.MethodPost, "/admin/api/accounts/reset",
		strings.NewReader(`{"accountId":"acc_reset"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body=%s", response.Code, response.Body.String())
	}

	acc = getAccountByID("acc_reset")
	if acc.UsageCount != 0 || acc.DailyUsageCount != 0 || acc.DailyUsageDate != "" {
		t.Fatalf("manual reset left counters: total=%d daily=%d date=%q",
			acc.UsageCount, acc.DailyUsageCount, acc.DailyUsageDate)
	}
	if acc.Status != "active" {
		t.Fatalf("reset status = %q, want active", acc.Status)
	}
}
