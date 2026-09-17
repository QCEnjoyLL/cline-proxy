package main

import (
	"strings"
	"testing"
)

// 生成的 Key 必须不可预测：旧实现用 UnixMilli + UnixNano%1e6 当熵源，
// 攻击者按已知生成时刻枚举即可撞出有效 Key。
//
// 这里只能验证“看起来是随机的”，无法证明密码学强度——真正的保证是 newAPIKey
// 走 crypto/rand（见实现）。本测试的价值在于锁住格式与唯一性，并防止有人
// 又把实现改回时间戳拼接（时间戳拼接会因同一毫秒内重复而撞出重复 Key）。
func TestNewAPIKeyIsRandomAndWellFormed(t *testing.T) {
	const n = 200
	seen := make(map[string]bool, n)

	for i := 0; i < n; i++ {
		key, err := newAPIKey()
		if err != nil {
			t.Fatalf("newAPIKey: %v", err)
		}
		if !strings.HasPrefix(key, apiKeyPrefix) {
			t.Fatalf("key %q missing prefix %q", key, apiKeyPrefix)
		}
		// prefix + hex(24 字节) = 6 + 48 字符。
		if want := len(apiKeyPrefix) + apiKeyRandomBytes*2; len(key) != want {
			t.Fatalf("key %q length = %d, want %d", key, len(key), want)
		}
		if seen[key] {
			t.Fatalf("duplicate key generated: %q (entropy source is not random?)", key)
		}
		seen[key] = true
	}
}

// 校验必须能命中、且必须拒绝一切近似值。
func TestValidAPIKeyAcceptsOnlyExactMatch(t *testing.T) {
	keys := []string{"cline_aaa", "cline_bbb"}

	for _, k := range keys {
		if !validAPIKey(keys, k) {
			t.Errorf("validAPIKey rejected configured key %q", k)
		}
	}

	for _, bad := range []string{
		"",
		"cline_",
		"cline_aab",  // 只差最后一个字节——旧的非恒定时间比较会在此提前返回
		"cline_aaa ", // 尾随空格
		"CLINE_AAA",  // 大小写不同
		"cline_aaaa",
	} {
		if validAPIKey(keys, bad) {
			t.Errorf("validAPIKey accepted wrong key %q", bad)
		}
	}

	// 未配置任何 Key 时不应命中；该场景的“放行”由调用方决定（见 apiKeyHandler），
	// 不能泄漏进这个判断里。
	if validAPIKey(nil, "anything") {
		t.Error("validAPIKey with no configured keys should not match")
	}
}

// 后台生成的 Key 必须能通过代理侧校验——“生成出来的 Key 用不了”是最容易被
// 改坏的一环（两边格式一旦漂移就会静默失效）。
func TestGeneratedKeyPassesProxyValidation(t *testing.T) {
	key, err := newAPIKey()
	if err != nil {
		t.Fatalf("newAPIKey: %v", err)
	}
	if !validAPIKey([]string{key}, key) {
		t.Errorf("generated key %q did not pass validAPIKey", key)
	}
}
