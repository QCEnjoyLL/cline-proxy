package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stats 里的 strategy 必须是真实配置，而不是写死的 round_robin。
//
// 背景：设置页用 GET /stats 的 strategy 回填「负载均衡策略」下拉框
// （web/admin.html: `if (s.strategy) _('settingStrategy').value = s.strategy;`）。
// 写死常量会让用户把策略改成 fill / random 后，页面仍然显示 round_robin。
func TestStatsReportsConfiguredStrategy(t *testing.T) {
	original := getProxyConfig()
	t.Cleanup(func() { setProxyConfig(original) })

	for _, want := range []string{"fill", "random"} {
		next := *original
		next.Strategy = want
		setProxyConfig(&next)

		rec := httptest.NewRecorder()
		handleAdminStats(rec, httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}

		var resp struct {
			Data struct {
				Strategy string `json:"strategy"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
		}
		if resp.Data.Strategy != want {
			t.Fatalf("strategy = %q, want %q", resp.Data.Strategy, want)
		}
	}
}

// 手动禁用的账号只计入 disabled，不再重复计入 active / expired，
// 否则各计数之和会大于账号总数，面板数字互相矛盾。
func TestStatsCountsDisabledOnce(t *testing.T) {
	useTemporaryPool(t)
	p := loadPool()
	poolMu.Lock()
	p.Accounts = []*Account{
		{AccountID: "a1", Status: "active"},
		{AccountID: "a2", Status: "expired"},
		{AccountID: "a3", Status: "active", Disabled: true},
	}
	poolMu.Unlock()

	rec := httptest.NewRecorder()
	handleAdminStats(rec, httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil))

	var resp struct {
		Data struct {
			Total    int `json:"total"`
			Active   int `json:"active"`
			Expired  int `json:"expired"`
			Disabled int `json:"disabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Data.Total != 3 || resp.Data.Active != 1 || resp.Data.Expired != 1 || resp.Data.Disabled != 1 {
		t.Fatalf("统计不一致：total=%d active=%d expired=%d disabled=%d",
			resp.Data.Total, resp.Data.Active, resp.Data.Expired, resp.Data.Disabled)
	}
}
