package main

import (
	"bytes"
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
		if !os.IsNotExist(err) {
			log.Printf("Failed to read accounts file %s: %v", poolPath, err)
		}
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
		return pool
	}

	// 去掉 UTF-8 BOM：Windows 记事本 / PowerShell 的 Set-Content -Encoding UTF8 /
	// 部分编辑器默认会加上它，而 encoding/json 对 BOM 直接报错。
	// 报错的后果极其严重——见下面解析失败分支。
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	// 空文件（或只有空白/BOM）不是损坏，直接当空池启动，不必留备份文件。
	if len(bytes.TrimSpace(data)) == 0 {
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
		return pool
	}

	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		// 绝不静默换成空池：只要进程继续跑，任何一次写操作（addAccount、
		// pickAccount 的用量落盘、改配置…）都会把账号文件覆盖成空池，
		// 用户的账号就永久没了。先把原文件改名留证，再以空池启动。
		backup := fmt.Sprintf("%s.corrupt-%d", poolPath, time.Now().Unix())
		if rerr := os.Rename(poolPath, backup); rerr != nil {
			log.Printf("Accounts file %s is not valid JSON (%v) and could not be backed up: %v", poolPath, err, rerr)
		} else {
			log.Printf("Accounts file %s is not valid JSON (%v); kept it as %s and started with an empty pool", poolPath, err, backup)
		}
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
		return pool
	}

	if p.Accounts == nil {
		p.Accounts = []*Account{}
	}
	if p.Keys == nil {
		p.Keys = []string{}
	}

	// 一次性迁移：旧版本把 429 限流写进账号级 Status="cooldown" 并持久化，
	// 而现在冷却已改为「账号×模型」级（见 cooldown.go），没有任何代码会把
	// Status 写回 "active"，而 pickAccount 又会跳过一切非 active 账号——
	// 若不迁移，升级前被标过 cooldown 的账号将永久不可用。
	for _, a := range p.Accounts {
		if a.Status == "cooldown" {
			a.Status = "active"
		}
	}

	pool = &p
	return pool
}

// saveMu 只序列化「写文件」这一步：两个 goroutine 同时写同一个文件会互相截断，
// 落盘结果变成两段 JSON 拼接，而 loadPool 对解析失败是静默换成空池，
// 下次启动就等于「账号全没了」。
var saveMu sync.Mutex

// marshalPool 把账号池序列化成 JSON。
//
// 必须在持有 poolMu 时调用：pool 是全局共享对象，其他 goroutine 会在池锁内
// append 切片、改字段，锁外读它的切片头属于数据竞争，可能撕出一个非法
// 指针直接 panic。
func marshalPool() ([]byte, error) {
	data, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal accounts: %v", err)
	}
	return data, err
}

// writePoolFile 把已序列化的内容原子地写入磁盘：先写临时文件，再 rename 覆盖。
//
// 旧实现直接 os.WriteFile（O_TRUNC），两个并发调用会互相截断；
// 原子替换消除了这种可能。临时文件名带 pid，避免两个进程共用同一个
// 账号池路径时互相踩对方的临时文件。
func writePoolFile(data []byte) error {
	saveMu.Lock()
	defer saveMu.Unlock()

	tmp := fmt.Sprintf("%s.%d.tmp", poolPath, os.Getpid())
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("Failed to save accounts: %v", err)
		return err
	}
	if err := os.Rename(tmp, poolPath); err != nil {
		log.Printf("Failed to replace accounts file: %v", err)
		os.Remove(tmp)
		return err
	}
	return nil
}

// savePool 在池锁内序列化、在池锁外落盘。
//
// 调用方不得持有 poolMu——这里会取它。两个锁从不嵌套（poolMu 只包 marshal，
// saveMu 只包 write），所以不存在锁序死锁；已经持有 poolMu 的调用点必须改用
// savePoolLocked，否则会自锁死。
func savePool() error {
	poolMu.Lock()
	data, err := marshalPool()
	poolMu.Unlock()
	if err != nil {
		return err
	}
	return writePoolFile(data)
}

// savePoolLocked 供已经持有 poolMu 的调用点使用（pickAccount、models.go 的写操作等）。
func savePoolLocked() error {
	data, err := marshalPool()
	if err != nil {
		return err
	}
	return writePoolFile(data)
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
			savePoolLocked()
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

// refreshAccountToken 刷新账号 token 并落盘。
//
// 网络请求在 poolMu 之外，字段写入在 poolMu 之内：savePool 自己会取 poolMu，
// 所以必须先解锁再落盘（持锁调用 savePool 会自锁死）。
func refreshAccountToken(acc *Account) error {
	resp, err := refreshClineToken(acc.RefreshToken)
	if err != nil {
		poolMu.Lock()
		acc.Status = "expired"
		poolMu.Unlock()
		savePool()
		return fmt.Errorf("token refresh failed: %w", err)
	}

	poolMu.Lock()
	acc.AccessToken = "workos:" + resp.Data.AccessToken
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}
	acc.ExpiresAt = parseExpiry(resp.Data.ExpiresAt) - 60000
	acc.Status = "active"
	poolMu.Unlock()

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

	savePoolLocked()
	return acc
}

func ensureAccountToken(acc *Account) (string, error) {
	poolMu.Lock()
	cached := acc.AccessToken != "" && time.Now().UnixMilli() < acc.ExpiresAt
	poolMu.Unlock()
	if cached {
		return acc.AccessToken, nil
	}

	if err := refreshAccountToken(acc); err != nil {
		return "", err
	}

	return acc.AccessToken, nil
}

// cooldownMinutes 返回当前生效的冷却时长（分钟），带默认值兜底。
// 存放在账号池文件里，因此跨重启保留。
func cooldownMinutes() int {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	if p.CooldownMinutes <= 0 {
		return defaultCooldownMinutes
	}
	return p.CooldownMinutes
}

// setCooldownMinutes 持久化冷却时长；写盘失败时回滚内存值。
func setCooldownMinutes(m int) error {
	p := loadPool()
	poolMu.Lock()
	prev := p.CooldownMinutes
	p.CooldownMinutes = m
	poolMu.Unlock()

	if err := savePool(); err != nil {
		// 回滚也要在锁内：池锁外的写入会与 marshal 竞争。
		poolMu.Lock()
		p.CooldownMinutes = prev
		poolMu.Unlock()
		return err
	}
	return nil
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
