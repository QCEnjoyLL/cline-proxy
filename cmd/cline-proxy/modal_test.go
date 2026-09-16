package main

import (
	"regexp"
	"strings"
	"testing"
)

// 弹层这类 position:fixed 的元素必须挂在 <body> 直属层。
//
// 背景：详情弹层的 div 一开始写在 #tab-accounts 里，而 tab 面板在未激活时是
// display:none —— fixed 定位的元素处于 display:none 的祖先下不会被渲染，
// 表现为「点了详情什么都没出现」。这类错误在源码里看结构很难发现。
func TestModalIsOutsideTabPanels(t *testing.T) {
	page := renderPage(adminHTML)

	modalIdx := strings.Index(page, `id="accountModal"`)
	if modalIdx < 0 {
		t.Fatal("页面里找不到账号详情弹层")
	}

	// 弹层必须出现在所有 tab 面板的内容之后（tab-panel 都在 .layout 内，
	// 而弹层在 .layout 之外，因此它后面不应再有 tab 面板的 .section 结构）。
	lastTabIdx := strings.LastIndex(page, `class="tab-panel"`)
	if modalIdx < lastTabIdx {
		t.Fatalf("弹层位于 tab 面板内部（弹层偏移 %d < 最后一个 tab-panel 偏移 %d）"+
			"：display:none 的祖先会让它完全不显示", modalIdx, lastTabIdx)
	}

	// 弹层必须出现在 #toast 附近（两者同为 body 直属子元素）
	toastIdx := strings.Index(page, `id="toast"`)
	if toastIdx < 0 {
		t.Fatal("页面里找不到 toast 容器")
	}
	if abs(modalIdx-toastIdx) > 4000 {
		t.Fatalf("弹层与 #toast 相距过远（%d 字符），可能又嵌进了某个 tab 面板", abs(modalIdx-toastIdx))
	}

	// 弹层内部不能出现 id 重复
	for _, id := range []string{"accountModal", "modalTitle", "modalSub", "modalBody"} {
		if n := strings.Count(page, `id="`+id+`"`); n != 1 {
			t.Errorf(`id="%s" 出现 %d 次（应为 1 次）`, id, n)
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// 受限模型表的关键列必须禁止折行，否则「23 小时 58 分」「2026/9/18 00:00:00」
// 会被挤成多行（这是用户实际反馈的问题）。
func TestModalTableColumnsDoNotWrap(t *testing.T) {
	page := renderPage(adminHTML)

	// 第 2~4 列（原因/剩余/重置时间）与最后一列（操作）应 nowrap
	needles := []string{
		"#modalBody table th:nth-child(2),#modalBody table td:nth-child(2)",
		"#modalBody table th:nth-child(3),#modalBody table td:nth-child(3)",
		"#modalBody table th:nth-child(4),#modalBody table td:nth-child(4)",
		"#modalBody table th:last-child,#modalBody table td:last-child",
	}
	for _, n := range needles {
		idx := strings.Index(page, n)
		if idx < 0 {
			t.Errorf("缺少选择器 %q", n)
			continue
		}
		// 取该规则到下一个 } 的内容，确认含 white-space:nowrap
		end := strings.Index(page[idx:], "}")
		if end < 0 {
			t.Errorf("选择器 %q 的规则没有闭合", n)
			continue
		}
		rule := page[idx : idx+end]
		if !strings.Contains(rule, "white-space:nowrap") {
			t.Errorf("规则 %q 未设置为不折行：%s", n, rule)
		}
	}

	// 弹层宽度应明显大于原来的 560px
	re := regexp.MustCompile(`\.modal-card\{[^}]*max-width:(\d+)px`)
	m := re.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("找不到 .modal-card 的 max-width")
	}
	if m[1] != "920" {
		t.Errorf(".modal-card max-width = %spx，期望 920px", m[1])
	}
}

// 「上游未给出重置时间」那段说明只能在确有此类条目时出现。
//
// 背景：原来只判断 limited.length > 0，于是所有条目都有明确重置时间时
// 也会显示「未给出重置时间时按 30 分钟恢复」，与表格里的具体时间自相矛盾。
func TestResetNoteOnlyWhenSomeEntryLacksReset(t *testing.T) {
	page := renderPage(adminHTML)

	if !strings.Contains(page, "limited.filter(c => !c.resetsAt)") {
		t.Fatal("说明文案应基于「确实没有重置时间的条目数」来决定是否显示")
	}
	if strings.Contains(page, "if (limited.length) {\n    html += '<div class=\"kv\"") {
		t.Fatal("仍在使用 limited.length 判断，会把有重置时间的条目也算进去")
	}
	// 新的说明文案应包含条目数量，便于对照
	if !strings.Contains(page, "项上游未给出重置时间") {
		t.Error("说明文案应写明有多少项没有重置时间")
	}
}