package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type imageRoundTrip func(*http.Request) (*http.Response, error)

func (f imageRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestImageGenerateRequestAndParse 锁定 generations 的 URL / 头 / 请求体 / 信封解析。
func TestImageGenerateRequestAndParse(t *testing.T) {
	var gotPath, gotAuth, gotAccept, gotCT, gotIntent, gotBody string
	c := &Client{
		HTTP: &http.Client{Transport: imageRoundTrip(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotAccept = r.Header.Get("Accept")
			gotCT = r.Header.Get("Content-Type")
			gotIntent = r.Header.Get("X-Agent-Intent")
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"code":0,"request_id":"rid-1","data":{"data":[{"url":"https://cdn.example/1.png"}],"usage":{"credit":7}}}`)),
			}, nil
		})},
		WebBaseCN: "https://img.example",
	}

	out, err := c.Image(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, ImageGeneration,
		map[string]any{"model": "hunyuan-image-v3.0", "prompt": "cat", "size": "512x512", "n": 2},
		ChatMeta{ConversationRequestID: "req-1"})
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	if gotPath != "/v2/images/generations" {
		t.Errorf("path=%q want /v2/images/generations", gotPath)
	}
	if gotAuth != "Bearer at" {
		t.Errorf("authorization=%q want Bearer at", gotAuth)
	}
	if gotAccept != "application/json" || gotCT != "application/json" {
		t.Errorf("accept=%q content-type=%q want application/json", gotAccept, gotCT)
	}
	if gotIntent != "craft" {
		t.Errorf("X-Agent-Intent=%q want craft", gotIntent)
	}
	if !strings.Contains(gotBody, `"model":"hunyuan-image-v3.0"`) || !strings.Contains(gotBody, `"n":2`) {
		t.Errorf("body=%s", gotBody)
	}
	if len(out.Items) != 1 || out.Items[0].URL != "https://cdn.example/1.png" {
		t.Fatalf("items=%+v", out.Items)
	}
	if !out.HasCredit || out.Credit != 7 {
		t.Errorf("credit=%v has=%v want 7/true", out.Credit, out.HasCredit)
	}
	if out.RequestID != "rid-1" {
		t.Errorf("requestID=%q", out.RequestID)
	}
}

// TestImageEditUsesEditsPath 图生图必须打 /v2/images/edits（独立端点，不是
// generations 的别名）。
func TestImageEditUsesEditsPath(t *testing.T) {
	var gotPath string
	c := &Client{
		HTTP: &http.Client{Transport: imageRoundTrip(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"data":[{"b64_json":"aGk="}]}}`)),
			}, nil
		})},
		WebBaseCN: "https://img.example",
	}
	out, err := c.Image(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, ImageEdit,
		map[string]any{"model": "hunyuan-image-alpha-edit", "prompt": "edit", "image": []string{"data:image/png;base64,aGk="}},
		ChatMeta{ConversationRequestID: "req-2"})
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	if gotPath != "/v2/images/edits" {
		t.Errorf("path=%q want /v2/images/edits", gotPath)
	}
	if len(out.Items) != 1 || out.Items[0].B64JSON != "aGk=" {
		t.Errorf("items=%+v", out.Items)
	}
}

// TestImageErrorsClassified 业务 code 与 HTTP 状态都要落到既有错误分类。
func TestImageErrorsClassified(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"图片无效 11135", http.StatusBadRequest, `{"code":11135,"msg":"invalid image data"}`, ErrImageInvalid},
		{"余额不足 200 信封", http.StatusOK, `{"code":14018,"msg":"积分不足"}`, ErrHardCredit},
		{"普通业务错误 200", http.StatusOK, `{"code":5000,"msg":"prompt rejected"}`, ErrClient},
		{"服务端错误", http.StatusInternalServerError, `{"msg":"boom"}`, ErrServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{
				HTTP: &http.Client{Transport: imageRoundTrip(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: tc.status,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(tc.body)),
					}, nil
				})},
				WebBaseCN: "https://img.example",
			}
			_, err := c.Image(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, ImageGeneration,
				map[string]any{"model": "hunyuan-image-v3.0", "prompt": "x"}, ChatMeta{ConversationRequestID: "r"})
			if err == nil {
				t.Fatal("want error")
			}
			ue, ok := err.(*Error)
			if !ok {
				t.Fatalf("err type=%T want *Error: %v", err, err)
			}
			if ue.Kind != tc.want {
				t.Errorf("kind=%v want %v (msg=%s)", ue.Kind, tc.want, ue.Msg)
			}
		})
	}
}
