package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestSortQueueByCredits 成长队列按余额升序（0 积分最前），同余额保持扫描序。
// 语义：队列是全量扫描 + 并发受限，排后面的号在本轮跑不完时会被顺延，所以顺序
// 直接决定"哪个号先恢复可用"。0 积分的号不能承载请求，必须最先跑。
func TestSortQueueByCredits(t *testing.T) {
	mk := func(uid string, credits int64) queueAccount {
		return queueAccount{a: &auth.Auth{UID: uid}, credits: credits}
	}
	accts := []queueAccount{
		mk("u-rich", 900),
		mk("u-zero-1", 0),
		mk("u-mid", 35),
		mk("u-zero-2", 0),
		mk("u-neg", -1), // 理论不会出现，但排序必须把它排最前而不是末尾
	}
	sortQueueByCredits(accts)

	want := []string{"u-neg", "u-zero-1", "u-zero-2", "u-mid", "u-rich"}
	for i, w := range want {
		if got := accts[i].a.UID; got != w {
			t.Fatalf("第 %d 位 = %s，want %s（全序：%v）", i, got, w, uidsOf(accts))
		}
	}
}

func uidsOf(accts []queueAccount) []string {
	out := make([]string, len(accts))
	for i, a := range accts {
		out[i] = a.a.UID
	}
	return out
}

// TestWriteStaticCacheHeaders 内嵌静态资源必须可校验：no-cache + ETag，
// 命中 If-None-Match 回 304。缺这层时浏览器启发式长期缓存旧 app.js，
// 升级后前端文案与后端不一致（2026-10-23 实际踩到）。
func TestWriteStaticCacheHeaders(t *testing.T) {
	body := []byte("console.log(1)")

	rec := httptest.NewRecorder()
	writeStatic(rec, httptest.NewRequest(http.MethodGet, "/panel/app.js", nil),
		"text/javascript; charset=utf-8", body)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("首次请求状态码 = %d，want 200", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q，want no-cache", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("缺 ETag：浏览器无法条件校验，会退回启发式缓存")
	}
	if rec.Body.String() != string(body) {
		t.Error("响应体与源文件不一致")
	}

	// 带同 ETag 再来一次 → 304 且无 body
	req2 := httptest.NewRequest(http.MethodGet, "/panel/app.js", nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	writeStatic(rec2, req2, "text/javascript; charset=utf-8", body)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("条件请求状态码 = %d，want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 不应带响应体，实际 %d 字节", rec2.Body.Len())
	}

	// 内容变化 → ETag 变 → 必须 200 回新内容（这才是修掉"旧 JS"的关键）
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/panel/app.js", nil)
	req3.Header.Set("If-None-Match", etag)
	writeStatic(rec3, req3, "text/javascript; charset=utf-8", []byte("console.log(2)"))
	if rec3.Code != http.StatusOK {
		t.Fatalf("内容变更后状态码 = %d，want 200", rec3.Code)
	}
	if rec3.Header().Get("ETag") == etag {
		t.Error("内容变了 ETag 却不变，浏览器会一直用旧脚本")
	}
}
