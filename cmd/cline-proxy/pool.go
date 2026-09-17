package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
		// 用户的账号就永久没了。先把原文件留一份再以空池启动。
		backup, berr := backupUnreadablePool()
		if berr != nil {
			// 连备份都做不成：此时绝不能继续，否则后续任意一次写操作都会
			// 把用户唯一的账号文件盖掉。宁可这次不落盘——账号文件原样留在
			// 磁盘上，进程带着空池跑，每次保存都返回错误（见 writePoolFile）。
			poolSaveBlocked.Store(fmt.Sprintf(
				"accounts file %s is not valid JSON (%v) and could not be backed up (%v); refusing to overwrite it",
				poolPath, err, berr))
			log.Printf("%s", poolSaveBlocked.Load())
			log.Printf("The file is still on disk unchanged. Fix or remove it, then restart.")
		} else {
			log.Printf("Accounts file %s is not valid JSON (%v); kept a copy at %s and started with an empty pool",
				poolPath, err, backup)
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

// poolSaveBlocked 非空时禁止任何落盘，值为原因。
//
// 唯一会设置它的场景：账号文件解析失败，且连副本都留不下来。
// 此时若继续保存，就会把用户唯一的账号文件覆盖成空池；宁可让保存全部失败，
// 让文件原样留在磁盘上等人处理。
var poolSaveBlocked atomic.Value

// backupUnreadablePool 为「读得出来但解析不了」的账号文件留一份副本，返回副本路径。
//
// 为什么用「复制 + 截断原文件」而不是 os.Rename：
// docker-compose 把账号池按单文件 bind mount 挂进容器时，rename 或移动一个挂载点
// 会被内核拒绝（EBUSY「device or resource busy」——与 writePoolFile 里那个提示同源）。
// 复制内容不受该限制，所以在绑定挂载下也留得下副本。
//
// 顺序很关键：先把原内容完整复制出去，再截断原文件。中间任何一步失败都返回错误，
// 不会留下「副本没写成、原文件却已经没了」的状态。
func backupUnreadablePool() (string, error) {
	backup := fmt.Sprintf("%s.corrupt-%d", poolPath, time.Now().Unix())

	src, err := os.Open(poolPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dst, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(backup)
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", err
	}

	// 副本已落盘，现在把坏内容从原路径清掉：否则它会以「空池」的形态继续存活，
	// 下一次保存就把它覆盖成真正的空池（等于账号丢失）。
	if err := os.Truncate(poolPath, 0); err != nil {
		return backup, fmt.Errorf("wrote backup %s but could not clear the bad file: %w", backup, err)
	}
	return backup, nil
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

// renameFile 是 os.Rename 的间接层，供测试模拟「rename 不被允许」的环境。
var renameFile = os.Rename

// atomicReplaceUnsupportedFor 记录「哪个路径不能原子替换」。
// 按路径记忆：换了路径要重新尝试原子替换。只在 saveMu 内读写。
var atomicReplaceUnsupportedFor string

// writePoolFile 把已序列化的内容写入磁盘。
//
// 首选原子替换：先写同目录临时文件，再 rename 覆盖。旧实现直接 os.WriteFile
// （O_TRUNC），两个并发调用会互相截断，落盘结果变成两段 JSON 拼接。
//
// 但原子替换在某些部署下不可用：docker-compose 把账号池按「单文件 bind mount」
// 挂进容器（./.cline-accounts.json:/app/.cline-accounts.json）时，rename 覆盖挂载点
// 会被内核拒绝，报 EBUSY「device or resource busy」，于是每一次保存都失败。
// 因此 rename 失败时退回原地写，并记住这个路径不支持原子替换，后续不再白写临时文件。
func writePoolFile(data []byte) error {
	saveMu.Lock()
	defer saveMu.Unlock()

	// 解析失败且留不下副本时禁止落盘：继续写下去会把用户唯一的账号文件
	// 覆盖成空池。让每次保存都失败、把原因说清楚，比悄悄丢数据强。
	if reason, _ := poolSaveBlocked.Load().(string); reason != "" {
		return fmt.Errorf("%s", reason)
	}

	if atomicReplaceUnsupportedFor != poolPath {
		tmp := fmt.Sprintf("%s.%d.tmp", poolPath, os.Getpid())
		if err := os.WriteFile(tmp, data, 0600); err != nil {
			log.Printf("Failed to write accounts temp file: %v", err)
			return err
		}
		err := renameFile(tmp, poolPath)
		if err == nil {
			return nil
		}
		os.Remove(tmp)
		atomicReplaceUnsupportedFor = poolPath
		// 这行只可能出现在「本进程第一次落盘」时：atomicReplaceUnsupportedFor
		// 已置位，之后直接走原地写，不会反复刷屏（见
		// TestBindMountFallbackLogsOnlyOnce）。
		//
		// 保留一行、不彻底静默：这是「原子替换未生效」的唯一线索，排查数据
		// 完整性问题时需要它。但措辞要明确是预期情况、无需处理，别写成报错
		// 的样子；同时也别断言原因一定是 bind mount——rename 失败也可能来自
		// 文件被占用等其它原因，所以这里只说常见成因。
		// 只取底层原因（EBUSY → "device or resource busy"）：os.Rename 返回的是
		// *os.LinkError，它的 Error() 会把源/目标路径再拼一遍，而 poolPath 上面
		// 已经打过了，重复的路径只会让这行更难读。
		reason := err
		if inner := errors.Unwrap(err); inner != nil {
			reason = inner
		}
		log.Printf("accounts file %s: atomic replace unavailable (%v); using in-place writes for this process."+
			" Normal for a single-file bind mount (docker-compose); no action needed.", poolPath, reason)
	}

	// 原地写：saveMu 保证本进程内不会有两个写入者。
	//
	// 刻意不用 os.WriteFile（它先 O_TRUNC 再写）：进程若在截断之后、写入完成
	// 之前被杀，会留下一个 0 字节文件——而空文件按「空池」处理（不是损坏，
	// 不会留备份），下一次保存就把真实账号彻底覆盖掉了。
	// 改成「先写、后截断」：中途被杀最坏是「新内容 + 旧内容尾巴」，解析必然
	// 失败，于是 loadPool 会把原文件备份成 .corrupt-* 而不是静默丢掉。
	f, err := os.OpenFile(poolPath, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		log.Printf("Failed to open accounts file: %v", err)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		log.Printf("Failed to write accounts: %v", err)
		return err
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		f.Close()
		log.Printf("Failed to truncate accounts file: %v", err)
		return err
	}
	if err := f.Close(); err != nil {
		log.Printf("Failed to close accounts file: %v", err)
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
		poolMu.Unlock()
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

	// CurrentIdx 变了要落盘，但**磁盘 I/O 必须放在池锁之外**：这个函数在每个
	// 代理请求上都会被调用，而池锁是所有请求共享的——持锁写盘会让所有并发请求
	// 排队等磁盘，上游一慢就把锁的放大效应放得很大。
	// 做法与 savePool 一致：锁内序列化（拿到的是一致快照），锁外写。
	data, err := marshalPool()
	poolMu.Unlock()

	if err == nil {
		if werr := writePoolFile(data); werr != nil {
			log.Printf("Failed to persist accounts after picking an account: %v", werr)
		}
	}
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

// accountStats 是在池锁内统计出来的账号概况。
//
// 存在的意义：调用方需要在**一次加锁**里同时拿到「活跃数」和「总数」。若分成
// 多次 loadPool()/len(p.Accounts) 在锁外读，就会与并发的 addAccount /
// removeAccount（它们在锁内 append / 重建切片，会换掉底层数组）竞争——读 slice
// header 本身就是 data race，可能撕出一个非法指针。
type accountStats struct {
	Total  int
	Active int
}

// snapshotAccountStats 在池锁内统计账号数。
func snapshotAccountStats() accountStats {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	s := accountStats{Total: len(p.Accounts)}
	for _, a := range p.Accounts {
		if a != nil && a.Status == "active" {
			s.Active++
		}
	}
	return s
}

// accountCount 在池锁内读账号总数。
//
// 供「只要个数」的调用方使用：直接 len(loadPool().Accounts) 是锁外读 slice
// header，会与锁内的 append / 重建切片竞争。
func accountCount() int {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	return len(p.Accounts)
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
