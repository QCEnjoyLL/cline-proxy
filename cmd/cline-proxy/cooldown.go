package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cooldownKey 生成「账号 × 模型」维度的冷却键。
//
// 背景：上游按「账号 + 模型」组合限流（每个模型额度独立计算），
// 旧实现收到 429 就把整个账号标成 cooldown 并踢出轮询，
// 导致模型 A 到上限后同账号的模型 B 也一起不可用。
// 这里把冷却粒度降到组合级，模型 A 冷却不影响模型 B。
func cooldownKey(accountID, modelID string) string {
	return accountID + "|" + modelID
}

// limitKind 是上游 429 的种类。
//
// 只有前三种是上游明确告知「额度用尽」；unknown 是我们按配置时长猜的
// （比如上游没给可识别的原因），两者在界面上要区分开，不能把猜测当事实。
type limitKind string

const (
	limitKindFreeDaily   limitKind = "free_daily"   // 免费模型的每日额度
	limitKindPassLimit   limitKind = "pass_limit"   // ClinePass 订阅额度
	limitKindSpendLimit  limitKind = "spend_limit"  // 账户/组织的周期花费上限
	limitKindUnknown     limitKind = "unknown"      // 无法识别原因
)

// limitInfo 是从 429 响应体里解析出的额度信息。
type limitInfo struct {
	Kind    limitKind `json:"kind"`
	Detail  string    `json:"detail,omitempty"`  // 上游原文（已截断），供人工核对
	ResetAt time.Time `json:"-"`                 // 上游给出的重置时刻；零值表示上游没说
}

// 上游 429 响应体里用于识别的标记。
//
// 这些字符串来自 Cline 官方客户端源码（sdk/packages/llms/src/providers/errors.ts）：
//
//	CLINE_FREE_MODEL_LIMIT_MARKER      = "free limit reached on model"
//	CLINE_FREE_MODEL_LIMIT_RETRY_MARKER= "try again in "
//	CLINE_PASS_LIMIT_MARKER            = "clinepass limit"
//	CLINE_PASS_LIMIT_PREFIX            = "you have reached your"
//	CLINE_PASS_LIMIT_SUFFIX            = "please try again later."
//
// 除此之外还有 SPEND_LIMIT_EXCEEDED（JSON 结构，带 limit_usd/spent_usd/resets_at）。
// 上游改文案时这里会退化成 unknown，但不会误报「额度用尽」。
const (
	markerFreeModelLimit = "free limit reached on model"
	markerRetryIn        = "try again in "
	markerPassLimit      = "clinepass limit"
	markerSpendLimit     = "spend_limit_exceeded"
)

// 上游 429 原文可能很长（含 stacktrace / HTML），只留足以判断的一段。
const maxLimitDetail = 300

// maxParsedCooldown 是解析出的重置时间能生效的上限。
//
// 防止上游给出一个荒唐的值（或解析出错）把某个组合锁死很久；
// 免费模型的每日额度最长也就到明天零点，24 小时足够覆盖。
const maxParsedCooldown = 24 * time.Hour

// 账号或模型为空时冷却无意义；同时给 map 一个上限，避免长期运行下无限增长。
const maxCooldownRecords = 2000

// parseLimitInfo 解析 429 响应体，判断是不是「额度用尽」以及何时重置。
//
// 纯字符串处理，不需要网络；上游响应体就是它的唯一输入。
func parseLimitInfo(body string, now time.Time, localMidnight bool) limitInfo {
	detail := strings.TrimSpace(body)
	if len(detail) > maxLimitDetail {
		detail = detail[:maxLimitDetail] + "..."
	}
	info := limitInfo{Kind: limitKindUnknown, Detail: detail}
	if detail == "" {
		return info
	}

	lower := strings.ToLower(detail)

	switch {
	case strings.Contains(lower, markerFreeModelLimit):
		info.Kind = limitKindFreeDaily
		// 免费模型按「自然日」计算，重置即下一个零点。
		// 上游在某些响应里也会附 "try again in X"，优先用它。
		if d, ok := parseRetryAfter(lower); ok {
			info.ResetAt = now.Add(d)
		} else if localMidnight {
			info.ResetAt = nextLocalMidnight(now)
		} else {
			info.ResetAt = now.Add(24 * time.Hour).Truncate(time.Hour)
		}
	case strings.Contains(lower, markerPassLimit):
		info.Kind = limitKindPassLimit
		// 订阅额度没有固定周期，上游只说 "please try again later."
		if d, ok := parseRetryAfter(lower); ok {
			info.ResetAt = now.Add(d)
		}
	case strings.Contains(lower, markerSpendLimit) || strings.Contains(lower, "spend limit"):
		info.Kind = limitKindSpendLimit
		if ts, ok := parseJSONTimeField(detail, "resets_at"); ok {
			info.ResetAt = ts
		} else if d, ok := parseRetryAfter(lower); ok {
			info.ResetAt = now.Add(d)
		}
	}

	// 上游给的重置时间必须落在合理区间，否则丢弃（退回按配置时长冷却）。
	if !info.ResetAt.IsZero() {
		if !info.ResetAt.After(now) || info.ResetAt.Sub(now) > maxParsedCooldown {
			info.ResetAt = time.Time{}
		}
	}
	return info
}

// nextLocalMidnight 返回下一个本地零点（免费模型每日额度的重置时刻）。
func nextLocalMidnight(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
}

var (
	// "try again in 2 hours 30 minutes" / "try again in 45 minutes" / "try again in 1 hour"
	retryWordsRe = regexp.MustCompile(`(\d+)\s*(hour|hr|minute|min|second|sec)`)
	// "resets_at":"2026-09-17T00:00:00.000Z" 或 "resets_at": "..." 或裸值
	jsonTimeRe = regexp.MustCompile(`"` + `resets_at` + `"\s*:\s*"([^"]+)"`)
)

// parseRetryAfter 解析 "try again in <时长>"。
//
// 支持官方文案里的英文写法（hour/minute/second，可带复数），也兼容 1h30m 之类缩写。
func parseRetryAfter(lower string) (time.Duration, bool) {
	idx := strings.Index(lower, markerRetryIn)
	if idx < 0 {
		return 0, false
	}
	tail := lower[idx+len(markerRetryIn):]
	// 只在这一小段里找，避免把后面的其它数字（如错误码）当成时长
	if len(tail) > 80 {
		tail = tail[:80]
	}

	matches := retryWordsRe.FindAllStringSubmatch(tail, -1)
	if len(matches) == 0 {
		return 0, false
	}

	var total time.Duration
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		switch {
		case strings.HasPrefix(m[2], "h"):
			total += time.Duration(n) * time.Hour
		case strings.HasPrefix(m[2], "min"):
			total += time.Duration(n) * time.Minute
		case strings.HasPrefix(m[2], "s"):
			total += time.Duration(n) * time.Second
		}
	}
	if total <= 0 {
		return 0, false
	}
	return total, true
}

// parseJSONTimeField 从 JSON 串里取一个 RFC3339 时间字段。
//
// 用手写正则而不是 json.Unmarshal：429 的响应体不保证是合法 JSON
// （可能是纯文本、被截断的片段，或带前缀的日志），解析失败不能影响冷却本身。
func parseJSONTimeField(body, field string) (time.Time, bool) {
	re := jsonTimeRe
	if field != "resets_at" {
		re = regexp.MustCompile(`"` + regexp.QuoteMeta(field) + `"\s*:\s*"([^"]+)"`)
	}
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, m[1])
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// cooldownRecord 是一条冷却记录的内部状态。
type cooldownRecord struct {
	Until time.Time  // 冷却截止时刻
	Email string     // 账号邮箱（展示用；账号 ID 对人不友好）
	Info  limitInfo  // 上游给出的额度信息
}

// cooldownStore 保存全部「账号×模型」冷却记录（内存态，进程重启即清空）。
//
// 刻意不持久化：重启后冷却清零是合理的——重启往往意味着运维干预，
// 让全部账号重新回到轮询比带着旧状态继续更符合预期；
// 且旧实现把状态写进 Account.Status 造成「永不下线」的坑，这里不重蹈覆辙。
type cooldownStore struct {
	mu      sync.Mutex
	records map[string]cooldownRecord // key = cooldownKey
}

var cooldowns = &cooldownStore{records: map[string]cooldownRecord{}}

// mark 把「账号 × 模型」放入冷却，到点自动恢复（见 isCooling）。
//
// 账号或模型为空时直接跳过：组合级冷却需要一个确定的模型标识，
// 否则会写入一个永远查不到的 key（如 acc|），静默失效。
func (c *cooldownStore) mark(accountID, email, modelID string, now time.Time, ttl time.Duration, info limitInfo) {
	if accountID == "" || modelID == "" || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// map 上限保护：超出时先清掉已过期的记录，仍然超出就放弃写入新记录
	// （宁可少一条展示数据，也不能让内存无限增长）。
	if len(c.records) >= maxCooldownRecords {
		c.pruneExpiredLocked(now)
		if len(c.records) >= maxCooldownRecords {
			return
		}
	}

	c.records[cooldownKey(accountID, modelID)] = cooldownRecord{
		Until: now.Add(ttl),
		Email: email,
		Info:  info,
	}
}

// markCooldown 计算本次冷却时长并标记，返回实际使用的时长（供日志）。
//
// TTL 不在这里读配置：cooldownMinutes() 会获取 poolMu，
// 若在持有 cooldowns.mu 时调用就形成锁嵌套。这里先取 TTL 再入锁。
//
// 若上游明确给出了重置时刻（免费模型的次日零点、花费上限的 resets_at），
// 就用它而不是配置里的固定时长——否则会早于真实重置时间把请求放回去，
// 立刻再撞一次 429。
func markCooldown(acc *Account, modelID string, info limitInfo) time.Duration {
	now := time.Now()

	ttl := time.Duration(cooldownMinutes()) * time.Minute
	if !info.ResetAt.IsZero() {
		ttl = info.ResetAt.Sub(now)
		if ttl > maxParsedCooldown {
			ttl = maxParsedCooldown
		}
	}
	if ttl <= 0 {
		ttl = time.Duration(cooldownMinutes()) * time.Minute
	}

	email := ""
	if acc != nil {
		email = acc.Email
	}
	cooldowns.mark(accountIDOf(acc), email, modelID, now, ttl, info)
	return ttl
}

// accountIDOf 取账号 ID，容忍 nil（避免调用方为 nil 时 panic）。
func accountIDOf(acc *Account) string {
	if acc == nil {
		return ""
	}
	return acc.AccountID
}

// isCooling 返回该组合是否仍在冷却中。
// 过期的记录顺带清除（惰性清理，避免 map 无限增长）。
func (c *cooldownStore) isCooling(accountID, modelID string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cooldownKey(accountID, modelID)
	rec, ok := c.records[key]
	if !ok {
		return false
	}
	if now.Before(rec.Until) {
		return true
	}
	delete(c.records, key)
	return false
}

// clearAccount 解除某账号全部模型的冷却（手动「启用账号」时调用）。
func (c *cooldownStore) clearAccount(accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := accountID + "|"
	for key := range c.records {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(c.records, key)
		}
	}
}

// clearModel 解除单个「账号 × 模型」的冷却，返回是否删掉了记录。
//
// 手动重试入口：用户自己知道这个模型现在能用，不该被我们猜的时长挡住。
func (c *cooldownStore) clearModel(accountID, modelID string) bool {
	if accountID == "" || modelID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cooldownKey(accountID, modelID)
	if _, ok := c.records[key]; !ok {
		return false
	}
	delete(c.records, key)
	return true
}

// clearAll 清空全部冷却（设置页修改冷却时长时调用，立即生效）。
func (c *cooldownStore) clearAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = map[string]cooldownRecord{}
}

// pruneExpiredLocked 清掉已过期的记录；调用方必须已持有 c.mu。
func (c *cooldownStore) pruneExpiredLocked(now time.Time) {
	for key, rec := range c.records {
		if !now.Before(rec.Until) {
			delete(c.records, key)
		}
	}
}

// cooldownEntry 是一条冷却记录的对外展示形态。
type cooldownEntry struct {
	AccountID    string `json:"accountId"`
	Email        string `json:"email"`
	ModelID      string `json:"modelId"`
	UntilUnixMs  int64  `json:"until"`
	RemainingSec int64  `json:"remainingSec"`
	// Limited 表示上游明确告知「额度用尽」（而不是我们按配置时长猜的 429）。
	Limited bool   `json:"limited"`
	Kind    string `json:"kind,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// ResetsAtUnixMs 是上游给出的重置时刻；0 表示上游没说。
	ResetsAtUnixMs int64 `json:"resetsAt,omitempty"`
}

// snapshot 返回仍在冷却中的条目（含剩余秒数），供管理面板展示。
//
// 按「重置时间升序」排列：最先恢复的排最前，这才是运维关心的顺序。
// 顺带完成惰性清理。
func (c *cooldownStore) snapshot(now time.Time) []cooldownEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := make([]cooldownEntry, 0, len(c.records))
	for key, rec := range c.records {
		if !now.Before(rec.Until) {
			delete(c.records, key)
			continue
		}
		// key 形如 "acc_xxx|vendor/model"；账号 ID 内不含 "|"（生成时用时间戳）
		accountID, modelID := splitCooldownKey(key)
		if accountID == "" {
			continue
		}
		entry := cooldownEntry{
			AccountID:    accountID,
			Email:        rec.Email,
			ModelID:      modelID,
			UntilUnixMs:  rec.Until.UnixMilli(),
			RemainingSec: int64(rec.Until.Sub(now) / time.Second),
			Limited:      rec.Info.Kind != limitKindUnknown && rec.Info.Kind != "",
			Kind:         string(rec.Info.Kind),
			Detail:       rec.Info.Detail,
		}
		if !rec.Info.ResetAt.IsZero() {
			entry.ResetsAtUnixMs = rec.Info.ResetAt.UnixMilli()
		}
		entries = append(entries, entry)
	}

	sortCooldownEntries(entries)
	return entries
}

// splitCooldownKey 把 "accountID|modelID" 拆开。
func splitCooldownKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return "", ""
}

// sortCooldownEntries 按恢复时间升序排列（同刻按账号/模型名稳定排序）。
func sortCooldownEntries(entries []cooldownEntry) {
	// 用插入排序：条目数量很小（冷却中的组合），且避免为此引入 sort 依赖。
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && cooldownLess(entries[j], entries[j-1]); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

func cooldownLess(a, b cooldownEntry) bool {
	if a.UntilUnixMs != b.UntilUnixMs {
		return a.UntilUnixMs < b.UntilUnixMs
	}
	if a.AccountID != b.AccountID {
		return a.AccountID < b.AccountID
	}
	return a.ModelID < b.ModelID
}

// cooldownEntriesFor 返回某账号的冷却条目（账号详情用）。
func (c *cooldownStore) entriesFor(accountID string, now time.Time) []cooldownEntry {
	all := c.snapshot(now)
	out := make([]cooldownEntry, 0, len(all))
	for _, e := range all {
		if e.AccountID == accountID {
			out = append(out, e)
		}
	}
	return out
}

// cooldownDetailBody 把上游原文当 JSON 解析，取出结构化字段（供详情展示）。
//
// 返回 nil 表示不是结构化 JSON（纯文本原因），此时前端只显示原文。
func cooldownDetailBody(detail string) map[string]any {
	if !strings.HasPrefix(strings.TrimSpace(detail), "{") {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(detail), &m); err != nil {
		return nil
	}
	// 上游把有用字段包在 error 里（{"error":{"code":...}}）
	if inner, ok := m["error"].(map[string]any); ok {
		return inner
	}
	return m
}

// formatResetAt 把重置时刻格式化成日志用的短字符串。
// 零值（上游没说）返回 "unknown"，避免日志里出现 0001-01-01。
func formatResetAt(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format("2006-01-02 15:04:05")
}