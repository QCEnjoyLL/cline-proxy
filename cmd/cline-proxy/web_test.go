package main

import (
	"regexp"
	"strings"
	"testing"
)

var (
	symbolDefRe  = regexp.MustCompile(`<symbol id="([^"]+)"`)
	iconUseRe    = regexp.MustCompile(`href="#(i-[a-z-]+)"`)
	htmlIDRe     = regexp.MustCompile(`\sid="([^"]+)"`)
	jsRefRe      = regexp.MustCompile(`_\(\s*'([A-Za-z][A-Za-z0-9_-]*)'\s*\)`)
	getByIDRe    = regexp.MustCompile(`getElementById\(\s*'([A-Za-z][A-Za-z0-9_-]*)'\s*\)`)
	querySelIDRe = regexp.MustCompile(`querySelector\(\s*'#([A-Za-z][A-Za-z0-9_-]*)'`)
)

func matchSet(re *regexp.Regexp, s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out[m[1]] = true
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 页面上用到的每个 SVG 图标都必须在 sprite 里定义。
//
// 背景：图标是 <svg><use href="#i-lock"/></svg> 引用的，符号名写错不会报任何错，
// 只会渲染成一个空白按钮——「禁用」按钮就这么空了一整个版本。
func TestAdminPageIconsAreDefined(t *testing.T) {
	pages := map[string]string{"web/admin.html": adminHTML, "web/login.html": adminLoginHTML}
	for name, page := range pages {
		defined := matchSet(symbolDefRe, page)
		used := matchSet(iconUseRe, page)
		for icon := range used {
			if !defined[icon] {
				t.Errorf("%s 引用了未定义的图标 #%s（可用 use 的图标：%s）",
					name, icon, strings.Join(keys(defined), ", "))
			}
		}
	}
}

// 页面上用 JS 取用的每个元素 id 都必须真的存在于 HTML 里。
//
// 背景：_('statCooldown') 这类写法拼错了只会静默拿到 null
// （或在 .textContent 上抛异常），页面看着正常但数据不更新。
func TestAdminPageJSIdsExist(t *testing.T) {
	pages := map[string]string{"web/admin.html": adminHTML, "web/login.html": adminLoginHTML}
	for name, page := range pages {
		ids := matchSet(htmlIDRe, page)
		for _, re := range []*regexp.Regexp{jsRefRe, getByIDRe, querySelIDRe} {
			for id := range matchSet(re, page) {
				if !ids[id] {
					t.Errorf("%s 的 JS 引用了不存在的元素 id %q", name, id)
				}
			}
		}
	}
}

// 主题切换按钮用 #i-sun / #i-moon 两个符号，二者都必须存在
// （它们是动态拼出来的 href，上面的静态扫描覆盖不到）。
func TestThemeToggleIconsAreDefined(t *testing.T) {
	defined := matchSet(symbolDefRe, adminHTML)
	for _, icon := range []string{"i-sun", "i-moon"} {
		if !defined[icon] {
			t.Errorf("缺少主题切换图标 #%s", icon)
		}
	}
}
