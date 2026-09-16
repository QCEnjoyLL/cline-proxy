package main

import (
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

// markCooldown 用当前生效的冷却时长标记「账号 × 模型」冷却，返回实际使用的时长（供日志）。
//
// TTL 不在这里读配置：cooldownMinutes() 会获取 poolMu，
// 若在持有 cooldowns.mu 时调用就形成锁嵌套。这里先取 TTL 再入锁。
func markCooldown(accountID, modelID string) time.Duration {
	ttl := time.Duration(cooldownMinutes()) * time.Minute
	cooldowns.mark(accountID, modelID, time.Now(), ttl)
	return ttl
}

// cooldownStore 保存全部「账号×模型」冷却记录（内存态，进程重启即清空）。
//
// 刻意不持久化：重启后冷却清零是合理的——重启往往意味着运维干预，
// 让全部账号重新回到轮询比带着旧状态继续更符合预期；
// 且旧实现把状态写进 Account.Status 造成「永不下线」的坑，这里不重蹈覆辙。
type cooldownStore struct {
	mu        sync.Mutex
	cooldowns map[string]time.Time // key = cooldownKey，value = 冷却截止时间
}

var cooldowns = &cooldownStore{cooldowns: map[string]time.Time{}}

// mark 把指定账号的指定模型放入冷却，到点自动恢复（见 isCooling）。
//
// 账号或模型为空时直接跳过：组合级冷却需要一个确定的模型标识，
// 否则会写入一个永远查不到的 key（如 acc|），静默失效。
func (c *cooldownStore) mark(accountID, modelID string, now time.Time, ttl time.Duration) {
	if accountID == "" || modelID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cooldowns[cooldownKey(accountID, modelID)] = now.Add(ttl)
}

// isCooling 返回该组合是否仍在冷却中。
// 过期的记录顺带清除（惰性清理，避免 map 无限增长）。
func (c *cooldownStore) isCooling(accountID, modelID string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cooldownKey(accountID, modelID)
	until, ok := c.cooldowns[key]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(c.cooldowns, key)
	return false
}

// clearAccount 解除某账号全部模型的冷却（手动「启用账号」时调用）。
func (c *cooldownStore) clearAccount(accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := accountID + "|"
	for key := range c.cooldowns {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(c.cooldowns, key)
		}
	}
}

// clearAll 清空全部冷却（设置页修改冷却时长时调用，立即生效）。
func (c *cooldownStore) clearAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cooldowns = map[string]time.Time{}
}

// cooldownEntry 是一条冷却记录的对外展示形态。
type cooldownEntry struct {
	AccountID    string `json:"accountId"`
	ModelID      string `json:"modelId"`
	UntilUnixMs  int64  `json:"until"`
	RemainingSec int64  `json:"remainingSec"`
}

// snapshot 返回仍在冷却中的条目（含剩余秒数），供管理面板展示。
// 顺带完成惰性清理。
func (c *cooldownStore) snapshot(now time.Time) []cooldownEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := make([]cooldownEntry, 0, len(c.cooldowns))
	for key, until := range c.cooldowns {
		if !now.Before(until) {
			delete(c.cooldowns, key)
			continue
		}
		// key 形如 "acc_xxx|vendor/model"；账号 ID 内不含 "|"（生成时用时间戳）
		var accountID, modelID string
		for i := 0; i < len(key); i++ {
			if key[i] == '|' {
				accountID, modelID = key[:i], key[i+1:]
				break
			}
		}
		if accountID == "" {
			continue
		}
		entries = append(entries, cooldownEntry{
			AccountID:    accountID,
			ModelID:      modelID,
			UntilUnixMs:  until.UnixMilli(),
			RemainingSec: int64(until.Sub(now) / time.Second),
		})
	}
	return entries
}
