// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//   - mobile.css  手机端（≤760px）适配样式（本仓库自有，上游不维护 → 同步上游不冲突）
//   - mobile.js   手机端联动逻辑（抽屉开合、宽表卡片化字段名），须在 app.js 之后加载
//   - favicon.svg 站点图标（WorkBuddy 官方 logo，取自 workbuddy.cn 的 <link rel=icon>）
//     单独成文件而不是内联 data URI：原图 4.1KB（含渐变 + 高斯模糊滤镜），
//     base64/百分号编码后会把 index.html 撑着，且改图就得改 HTML。
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strings"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

//go:embed mobile.css
var mobileCSS []byte

//go:embed mobile.js
var mobileJS []byte

//go:embed favicon.svg
var faviconSVG []byte

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js / mobile.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// writeStatic 输出内嵌静态资源，带 ETag + Cache-Control: no-cache。
//
// 为什么必须这么做：这些资源名是固定的（app.js / mobile.js，CSP 只允许同源固定名，
// 没有内容哈希后缀），而先前**一个缓存头都不发**，浏览器就按启发式规则长期缓存，
// 升级后仍然跑旧 app.js —— 面板出现「新后端 + 旧前端」的错位，例如已被修掉的企业
// 切换徽标文案（2026-10-23 实测：线上 1.13.8 已改中性标注，用户侧仍显示旧的
// 「企业切换·不计分」）。no-cache 的语义是「可缓存、但每次使用前必须校验」，
// 配合 ETag 命中即回 304，代价只是一次条件请求。
func writeStatic(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// versionedIndexHTML 给内嵌页面的资源引用打上版本号查询串（app.js?v=<版本>）。
//
// 为什么光有 ETag 不够：ETag 只在浏览器**愿意发条件请求**时才起作用。旧版本一个
// 缓存头都不发，已经进过用户缓存的 app.js 可能压根不再回源，于是升级后页面还是旧的
// （2026-10-23 连踩两次：企业切换徽标撤掉后仍有人看得到）。资源名带版本号是唯一
// 不依赖浏览器合作的做法 —— URL 变了就是新资源，缓存必然失效。
// 查询串不影响路由：mux 按路径匹配，appScript 照常命中。
func versionedIndexHTML(ver string) []byte {
	if ver == "" {
		return indexHTML
	}
	out := indexHTML
	// 只改属性值，不碰正文里提到文件名的注释（如「由 app.js 切 .on」）。
	// favicon 两个都要：<link href> 指标签页图标，<img src> 指侧栏品牌标。
	for _, ref := range []string{`src="app.js"`, `src="mobile.js"`, `href="mobile.css"`,
		`href="favicon.svg"`, `src="favicon.svg"`} {
		out = bytes.ReplaceAll(out, []byte(ref), []byte(ref[:len(ref)-1]+"?v="+ver+`"`))
	}
	return out
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	writeStatic(w, r, "text/html; charset=utf-8", versionedIndexHTML(p.cfg.Version))
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	writeStatic(w, r, "text/javascript; charset=utf-8", appJS)
}

// mobileStyle 输出手机端适配样式（同源样式表，CSP style-src 'self' 加载）。
// 单独成文而非写进 index.html：上游频繁改 index.html，独立文件可避免同步上游时反复冲突。
func (p *Panel) mobileStyle(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	writeStatic(w, r, "text/css; charset=utf-8", mobileCSS)
}

// favicon 输出站点图标（同源 SVG，CSP img-src 'self' 允许）。
// Content-Type 必须是 image/svg+xml：配 nosniff 时浏览器不会猜类型，给错就整张不显示。
func (p *Panel) favicon(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	writeStatic(w, r, "image/svg+xml", faviconSVG)
}

// mobileScript 输出手机端联动逻辑（同源脚本，须在 app.js 之后加载）。
func (p *Panel) mobileScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	writeStatic(w, r, "text/javascript; charset=utf-8", mobileJS)
}
