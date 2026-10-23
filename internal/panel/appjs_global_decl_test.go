package panel

import (
	"regexp"
	"strings"
	"testing"
)

// TestAppJSModuleGlobalsDeclared 防止「删了声明、留着赋值」这类事故复现。
//
// 背景（2026-10-23 线上故障）：6ff601b 撤「企业切换」徽标时，把
//
//	let entSwitch = new Set(), entSession = new Set();
//
// 整行删掉，但 loadOverview 里的 `entSession = new Set(...)` 和 renderAccounts 里的
// `entSession.has(...)` 都留着。app.js 顶部是 'use strict'，给未声明变量赋值直接抛
// ReferenceError；而 loadOverview 的 catch 在 quiet 轮询时静默吞异常，于是
// 概览接口 200、账号池却永远空白 —— 线上表现为「看不到账号池/日志」，无任何报错。
//
// 语法检查（node --check）和顶层冒烟都抓不到：前者不管引用，后者不跑异步响应分支。
// 这里做静态断言：模块级（缩进 ≤4）任何 `xxx = new Set(` / `new Map(` 的变量，
// 全文必须存在对应的 let/const/var 声明。
func TestAppJSModuleGlobalsDeclared(t *testing.T) {
	src := string(appJS)
	assign := regexp.MustCompile(`(?m)^[ \t]{0,4}([A-Za-z_$][\w$]*)\s*=\s*new\s+(?:Set|Map)\s*\(`)
	seen := map[string]bool{}
	for _, m := range assign.FindAllStringSubmatch(src, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		decl := regexp.MustCompile(`\b(?:let|const|var)\s+[^;\n]*\b` + regexp.QuoteMeta(name) + `\b`)
		if !decl.MatchString(src) {
			t.Errorf("app.js 里 %q 有模块级赋值但全文找不到声明（严格模式下会抛 ReferenceError，"+
				"且常被 quiet catch 静默吞掉）。请补 `let %s = ...`。", name, name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("没扫到任何 Set/Map 模块级赋值 —— 正则可能失效，请检查测试本身")
	}
}

// TestAppJSEntSessionDeclared entSession 是上面那条通用规则的具体回归锚点：
// 它在 renderAccounts 的模板串里被调用 .has()，缺声明时整张账号表渲染不出来。
func TestAppJSEntSessionDeclared(t *testing.T) {
	src := string(appJS)
	if !strings.Contains(src, "let entSession") {
		t.Fatal("app.js 缺少 `let entSession` 声明：loadOverview 会被 ReferenceError 打断，账号池空白")
	}
}
