package panel

import (
	"bytes"
	"testing"
)

// TestVersionedIndexHTML 页面里的资源引用必须带版本号查询串。
//
// 为什么单独测：ETag 依赖浏览器主动回源校验，旧版缓存过 app.js 的浏览器可能压根
// 不再请求；URL 带版本号是唯一不依赖浏览器合作的方式（2026-10-23 徽标撤掉后仍被
// 看到，就是栽在这上面）。这里同时钉住"只改属性值、不改正文注释"这条边界。
func TestVersionedIndexHTML(t *testing.T) {
	const ver = "9.9.9-panel"
	out := versionedIndexHTML(ver)

	for _, want := range []string{
		`src="app.js?v=` + ver + `"`,
		`src="mobile.js?v=` + ver + `"`,
		`href="mobile.css?v=` + ver + `"`,
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("页面缺少带版本号的引用：%s", want)
		}
	}

	// 正文里的注释提到了 app.js，不能被误改成 app.js?v=...（否则文案会变形）
	if bytes.Contains(out, []byte("由 app.js?v=")) {
		t.Error("注释里的文件名被误改了：只应替换属性值")
	}

	// 版本号为空时原样返回（单测/未注入版本的场景）
	if !bytes.Equal(versionedIndexHTML(""), indexHTML) {
		t.Error("空版本号应原样返回 indexHTML")
	}

	// 不同版本必须产生不同 URL，否则升级后旧缓存仍然命中
	other := versionedIndexHTML("9.9.10-panel")
	if bytes.Equal(out, other) {
		t.Error("不同版本产出的页面相同，缓存无法失效")
	}
}
