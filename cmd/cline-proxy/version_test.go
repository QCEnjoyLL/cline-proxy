package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestVersionLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.2.0", "v1.2.0"},
		{"v1.2.0", "v1.2.0"}, // 已经带前缀时不能变成 vv1.2.0
		{"v1.2.3-rc1", "v1.2.3-rc1"},
		{"1.2.3+build7", "v1.2.3+build7"},
	}
	original := Version
	t.Cleanup(func() { Version = original })

	for _, c := range cases {
		Version = c.in
		if got := versionLabel(); got != c.want {
			t.Errorf("Version=%q 时 versionLabel() = %q, want %q", c.in, got, c.want)
		}
	}
}

// 页面里必须用占位符而不是写死的版本号——写死就会和 Version 脱钩。
func TestPagesUseVersionPlaceholder(t *testing.T) {
	for name, page := range map[string]string{"web/admin.html": adminHTML, "web/login.html": adminLoginHTML} {
		if !strings.Contains(page, versionPlaceholder) {
			t.Errorf("%s 里没有版本号占位符 %s", name, versionPlaceholder)
		}
	}
}

// 返回给浏览器的页面里不能残留占位符，而且左下角要显示当前版本。
func TestServedPagesShowVersion(t *testing.T) {
	original := Version
	Version = "9.9.9"
	t.Cleanup(func() { Version = original })

	for name, page := range map[string]string{"web/admin.html": adminHTML, "web/login.html": adminLoginHTML} {
		got := renderPage(page)
		if strings.Contains(got, versionPlaceholder) {
			t.Errorf("%s 返回的内容里还残留占位符 %s", name, versionPlaceholder)
		}
		if !strings.Contains(got, "v9.9.9") {
			t.Errorf("%s 里没有出现版本号 v9.9.9", name)
		}
	}
}

// 面板左下角那个元素的文本必须就是版本号本身（而不是空着或带着别的东西）。
func TestAdminFooterVersionElement(t *testing.T) {
	original := Version
	Version = "9.9.9"
	t.Cleanup(func() { Version = original })

	re := regexp.MustCompile(`id="appVersion"[^>]*>([^<]*)<`)
	m := re.FindStringSubmatch(renderPage(adminHTML))
	if m == nil {
		t.Fatal("面板里找不到 id=\"appVersion\" 的元素")
	}
	if got := strings.TrimSpace(m[1]); got != "v9.9.9" {
		t.Fatalf("左下角显示的是 %q，want %q", got, "v9.9.9")
	}
}

// 接口里的版本号也必须是同一个来源。
func TestAPIsReportVersion(t *testing.T) {
	original := Version
	Version = "9.9.9"
	t.Cleanup(func() { Version = original })

	t.Run("stats", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handleAdminStats(rec, httptest.NewRequest(http.MethodGet, "/admin/api/stats", nil))

		var resp struct {
			Data struct {
				Version string `json:"version"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
		}
		if resp.Data.Version != "v9.9.9" {
			t.Fatalf("stats.version = %q, want v9.9.9", resp.Data.Version)
		}
	})

	t.Run("config", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handleAdminConfig(rec, httptest.NewRequest(http.MethodGet, "/admin/api/config", nil))

		var resp struct {
			Data struct {
				Version string `json:"version"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
		}
		if resp.Data.Version != "v9.9.9" {
			t.Fatalf("config.version = %q, want v9.9.9", resp.Data.Version)
		}
	})
}
