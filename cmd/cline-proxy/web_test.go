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

var (
	// onclick="..." 属性整体：从属性开始到它的闭合引号为止。
	// 属性值里只可能出现 \' 这种转义单引号，不会出现裸 "，所以 [^"]* 正好截到属性末尾。
	onclickAttrRe = regexp.MustCompile(`onclick="([^"]*)"`)
	// esc( 作为独立函数调用。不能用 strings.Contains：toggleModelDesc() 里也含
	// "esc(" 子串，会误报。
	escCallRe = regexp.MustCompile(`\besc\(`)
	// jsStr( 作为独立函数调用。
	jsStrCallRe = regexp.MustCompile(`\bjsStr\(`)
)

// onclick 里的值内插必须走 jsStr，不能走 esc。
//
// 背景：esc() 是基于 textContent 的 HTML 转义，只处理 & < >，**不处理引号和反斜杠**。
// 而 onclick="fn('...')" 是「HTML 属性里再嵌 JS 字符串」的双层上下文：
// ' 能结束 JS 字面量、\ 能吃掉转义、" 能提前闭合属性。曾有多处直接用 esc()，
// 一个含引号的模型 ID / API Key 即可注入 JS。
//
// 判据必须落在「属性内部」而不是「整行」：同一行里 jsStr（拼 onclick）与 esc
// （渲染旁边的纯文本节点）并存是正常的，按整行判断会漏掉
// `onclick="... jsStr(a) ... esc(b) ..."` 这种真回归——这一点是先用突变测试
// 把 jsStr(c.modelId) 改回 esc(c.modelId) 验证出来的。
func TestOnclickInterpolationDoesNotUseEsc(t *testing.T) {
	dynamic := 0
	for _, line := range strings.Split(adminHTML, "\n") {
		for _, m := range onclickAttrRe.FindAllStringSubmatch(line, -1) {
			region := m[1]
			// 静态处理器（如 toggleModelDesc()）没有内插，不存在转义问题。
			if !strings.Contains(region, "' +") {
				continue
			}
			dynamic++
			if escCallRe.MatchString(region) {
				t.Errorf("onclick 里的 JS 字符串内插用了 esc（只转 HTML，不挡引号），应改用 jsStr：\n  %s",
					strings.TrimSpace(line))
				continue
			}
			if !jsStrCallRe.MatchString(region) {
				t.Errorf("onclick 里的动态内插没有走 jsStr（缺少转义）：\n  %s", strings.TrimSpace(line))
			}
		}
	}
	// 若正则失效（比如页面改成别的事件属性写法），这条测试会静默失去意义。
	if dynamic == 0 {
		t.Fatal("没有匹配到任何 onclick 里的动态内插——正则或页面结构已变，本测试失去意义")
	}
}

// jsStr / attr 必须定义存在：上面那条测试会把 onclick 改成 jsStr，若函数名拼错
// 或整个被删掉，页面会在运行时报 ReferenceError，而静态 HTML 测试发现不了。
func TestPageDefinesEscapeHelpers(t *testing.T) {
	for _, fn := range []string{"const esc ", "const attr ", "const jsStr "} {
		if !strings.Contains(adminHTML, fn) {
			t.Errorf("admin.html 缺少转义辅助函数定义：%q", strings.TrimSpace(fn))
		}
	}
}
