package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestFetchRequestAndParse 抓取请求路径/请求体/扁平响应解析。
func TestFetchRequestAndParse(t *testing.T) {
	var gotPath, gotBody string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResp(200, `{"url":"https://mp.weixin.qq.com/s/x","title":"一篇公众号",
			"content":"正文内容","content_type":"text/html","content_length":4,
			"truncated":false,"status_code":200}`), nil
	})
	out, err := c.Fetch(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"},
		"https://mp.weixin.qq.com/s/x", "", ChatMeta{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/agenttool/v1/webfetch" {
		t.Errorf("path=%q", gotPath)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body=%s", gotBody)
	}
	if body["url"] != "https://mp.weixin.qq.com/s/x" {
		t.Errorf("url=%q", body["url"])
	}
	if body["prompt"] == "" {
		t.Error("空 prompt 必须补默认指令，否则上游 schema 校验会 400")
	}
	if out.Title != "一篇公众号" || out.Content != "正文内容" {
		t.Errorf("out=%+v", out)
	}
	if out.ContentLength != 4 {
		t.Errorf("content_length=%d", out.ContentLength)
	}
}

// TestFetchBusinessError 200 + code!=0 归为请求级错误（换号无用）。
func TestFetchBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":15003,"msg":"unsupported url"}`), nil
	})
	_, err := c.Fetch(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"},
		"https://example.com/a", "x", ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrClient {
		t.Fatalf("err=%v want ErrClient", err)
	}
}

// TestFetchEmptyContent 上游 200/code=0 但正文为空：按请求级错误返回，
// 不罚账号（同一页面换号也拿不到内容）。
func TestFetchEmptyContent(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"url":"https://example.com/a","content":"","status_code":403}`), nil
	})
	_, err := c.Fetch(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"},
		"https://example.com/a", "x", ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrClient {
		t.Fatalf("err=%v want ErrClient", err)
	}
	if !strings.Contains(ue.Msg, "empty content") {
		t.Errorf("msg=%q", ue.Msg)
	}
}

// TestValidateFetchURL 只接受 http/https 且必须有 host。
func TestValidateFetchURL(t *testing.T) {
	ok := []string{"https://example.com", "http://mp.weixin.qq.com/s/x"}
	for _, u := range ok {
		if _, err := ValidateFetchURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{"", "ftp://example.com", "file:///etc/passwd", "https://", "javascript:alert(1)"}
	for _, u := range bad {
		if _, err := ValidateFetchURL(u); err == nil {
			t.Errorf("%s: want error", u)
		}
	}
}
