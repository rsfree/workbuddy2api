package panel

import (
	"regexp"
	"strings"
	"testing"
)

// TestAppJSElementIDsExist app.js 里每个 $('id') 引用的元素都必须真的存在于
// index.html。
//
// 为什么值得一条测试：这些绑定是**模块顶层**语句，`$('x')` 拿到 null 时
// `.onclick = ...` 直接抛 TypeError，而它之后的全部顶层代码（其余按钮绑定、初始化、
// 轮询启动）都不会执行——现象是页面半死（部分按钮没反应），控制台只有一行报错，
// 很容易被当成"面板又抽风"。新增按钮时「只加了 JS 忘了加 HTML」「改 id 只改一边」
// 都会踩到，而 node --check 与 Go 编译都不管这种跨文件一致性。
func TestAppJSElementIDsExist(t *testing.T) {
	src := string(appJS)
	html := string(indexHTML)
	re := regexp.MustCompile(`\$\('([A-Za-z0-9_]+)'\)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("app.js 引用了 #%s，但 index.html 里没有这个元素：顶层绑定会抛 TypeError，其后代码全部不执行", id)
		}
	}
	if len(seen) < 50 {
		t.Fatalf("只扫到 %d 个 id 引用 —— 正则可能失效，请检查测试本身", len(seen))
	}
}

// TestPruneButtonsWired 两类批量剔除的按钮必须成对存在（HTML + JS 绑定）。
//
// 「剔除封禁号」与「剔除失效号」是账号池里仅有的两个破坏性入口，缺一半的后果是
// 运维在脏号堆积时找不到出口（或者按钮点下去没反应）。后端路由由
// prune_banned_test.go / prune_session_dead_test.go 端到端覆盖，这里只钉前端配对。
func TestPruneButtonsWired(t *testing.T) {
	src := string(appJS)
	html := string(indexHTML)
	for _, id := range []string{"btnPruneBanned", "btnPruneDead"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少按钮 #%s", id)
		}
		if !strings.Contains(src, `$('`+id+`').onclick`) {
			t.Errorf("app.js 没有为 #%s 绑定点击事件", id)
		}
	}
	// 两个按钮共用同一套两段式流程：任一退化回"点一下就删"都是不可接受的。
	for _, ep := range []string{"accounts/prune_banned", "accounts/prune_session_dead"} {
		if !strings.Contains(src, `pruneFlow('`+ep+`'`) {
			t.Errorf("app.js 未通过 pruneFlow 调用 %s（两段式确认是硬要求）", ep)
		}
	}
}
