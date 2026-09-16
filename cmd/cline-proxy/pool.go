package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	pool      *AccountPool
	poolMu    sync.Mutex
	poolPath  string
)

func init() {
	exe, _ := os.Executable()
	poolPath = filepath.Join(filepath.Dir(exe), ".cline-accounts.json")
}

func loadPool() *AccountPool {
	poolMu.Lock()
	defer poolMu.Unlock()

	if pool != nil {
		return pool
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
		return pool
	}

	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
		return pool
	}

	if p.Accounts == nil {
		p.Accounts = []*Account{}
	}
	if p.Keys == nil {
		p.Keys = []string{}
	}
	pool = &p
	return pool
}

func savePool() error {
	data, _ := json.MarshalIndent(pool, "", "  ")
	if err := os.WriteFile(poolPath, data, 0600); err != nil {
		log.Printf("Failed to save accounts: %v", err)
		return err
	}
	return nil
}

func addAccount(acc *Account) {
	p := loadPool()
	poolMu.Lock()
	p.Accounts = append(p.Accounts, acc)
	poolMu.Unlock()
	savePool()
}

func removeAccount(accountID string) bool {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for i, a := range p.Accounts {
		if a.AccountID == accountID {
			p.Accounts = append(p.Accounts[:i], p.Accounts[i+1:]...)
			savePool()
			return true
		}
	}
	return false
}

func getAccountByID(accountID string) *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for _, a := range p.Accounts {
		if a.AccountID == accountID {
			return a
		}
	}
	return nil
}

func refreshAccountToken(acc *Account) error {
	resp, err := refreshClineToken(acc.RefreshToken)
	if err != nil {
		acc.Status = "expired"
		savePool()
		return fmt.Errorf("token refresh failed: %w", err)
	}

	acc.AccessToken = "workos:" + resp.Data.AccessToken
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}
	acc.ExpiresAt = parseExpiry(resp.Data.ExpiresAt) - 60000
	acc.Status = "active"
	savePool()
	return nil
}

// pickAccount 从池中选一个可用账号。
//
// modelID 用于「账号×模型」级冷却过滤：某账号的该模型在冷却中就跳过，
// 同账号的其它模型不受影响。modelID 为空时只按账号级状态过滤。
// 手动禁用（Disabled）的账号任何模型都不参与轮询。
func pickAccount(modelID string) *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	now := time.Now()
	active := make([]*Account, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		if a.Disabled {
			continue // 用户手动禁用：不自动恢复
		}
		if a.Status != "active" {
			continue // expired 等账号级故障仍跳过
		}
		if modelID != "" && cooldowns.isCooling(a.AccountID, modelID, now) {
			continue // 该模型的额度在冷却中，换下一个账号
		}
		active = append(active, a)
	}

	if len(active) == 0 {
		return nil
	}

	cfg := getProxyConfig()

	var acc *Account
	switch cfg.Strategy {
	case "fill":
		// Always pick the first available (fill)
		acc = active[0]
	case "random":
		// Random selection
		n := time.Now().UnixNano() % int64(len(active))
		acc = active[n]
	default: // round_robin
		if p.CurrentIdx >= len(active) {
			p.CurrentIdx = 0
		}
		acc = active[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(active)
	}

	savePool()
	return acc
}

func ensureAccountToken(acc *Account) (string, error) {
	if acc.AccessToken != "" && time.Now().UnixMilli() < acc.ExpiresAt {
		return acc.AccessToken, nil
	}

	if err := refreshAccountToken(acc); err != nil {
		return "", err
	}

	return acc.AccessToken, nil
}

func listAccounts() []*Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	today := currentDateKey(time.Now())
	result := make([]*Account, len(p.Accounts))
	for i, a := range p.Accounts {
		dailyUsageCount := a.DailyUsageCount
		if a.DailyUsageDate != today {
			dailyUsageCount = 0
		}
		// Don't expose tokens
		result[i] = &Account{
			AccountID:       a.AccountID,
			Email:           a.Email,
			Status:          a.Status,
			Disabled:        a.Disabled, // 必须带上，否则前端看不到手动禁用状态
			LastUsed:        a.LastUsed,
			UsageCount:      a.UsageCount,
			DailyUsageCount: dailyUsageCount,
			DailyUsageDate:  a.DailyUsageDate,
			CreatedAt:       a.CreatedAt,
		}
	}
	return result
}

// currentDateKey returns the local calendar date for usage bucketing. Stored
// counters roll over on the next usage; account listings report stale ones as zero.
func currentDateKey(now time.Time) string {
	return now.Format("2006-01-02")
}

// bumpAccountUsage counts one successful request. The daily counter resets
// automatically when the stored date no longer matches the current local
// date, while UsageCount keeps accumulating for the account's lifetime.
// Callers must hold poolMu.
func bumpAccountUsage(acc *Account, now time.Time) {
	today := currentDateKey(now)
	if acc.DailyUsageDate != today {
		acc.DailyUsageDate = today
		acc.DailyUsageCount = 0
	}
	acc.DailyUsageCount++
	acc.UsageCount++
	acc.LastUsed = now
}

func addAccountFromDeviceAuth() (*Account, error) {
	fmt.Print("\n=== Add New Cline Account (OAuth) ===\n\n")

	device, err := workosDeviceAuth()
	if err != nil {
		return nil, err
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	fmt.Println("  1. Open this URL in your browser:")
	fmt.Println("     " + authURL)
	fmt.Println("  2. Enter code: " + device.UserCode)
	fmt.Print("  3. Log in with Google, GitHub, or email\n\n")

	_ = openBrowser(authURL)
	fmt.Println("  Waiting for authorization...")

	interval := device.Interval
	if interval < 5 {
		interval = 5
	}
	expiresIn := device.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}

	workosTok, err := pollWorkosToken(device.DeviceCode, interval, expiresIn)
	if err != nil {
		return nil, err
	}

	fmt.Println("  WorkOS authorized. Registering with Cline...")

	cline, err := registerWithCline(workosTok.AccessToken, workosTok.RefreshToken)
	if err != nil {
		return nil, err
	}

	if cline.Data.RefreshToken == "" {
		return nil, fmt.Errorf("cline registration missing refresh token")
	}

	email := "unknown"
	if cline.Data.UserInfo != nil && cline.Data.UserInfo.Email != "" {
		email = cline.Data.UserInfo.Email
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        email,
		RefreshToken: cline.Data.RefreshToken,
		AccessToken:  "workos:" + cline.Data.AccessToken,
		ExpiresAt:    parseExpiry(cline.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}

	addAccount(acc)
	fmt.Printf("  Account added! Email: %s\n", email)
	return acc, nil
}
