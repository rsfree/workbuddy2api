package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestSubmitVideoBody 提交请求体必须与官方客户端同形：
// image_url / reference_images[{image_url}] / extra_parameters{resolution,...}。
func TestSubmitVideoBody(t *testing.T) {
	var gotPath string
	var body map[string]any
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("body=%s", raw)
		}
		return jsonResp(200, `{"code":0,"requestId":"req-1","data":{"id":"task-abc"}}`), nil
	})
	audio := true
	out, err := c.SubmitVideo(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, VideoSubmitInput{
		Model:           "minimax-video-h3",
		Prompt:          "一只猫在雪地里奔跑",
		Seconds:         5,
		ImageURL:        "https://cdn.example/first.png",
		ReferenceImages: []string{"https://cdn.example/ref1.png", "https://cdn.example/ref2.png"},
		Resolution:      "768P",
		AspectRatio:     "16:9",
		EnableAudio:     &audio,
	}, ChatMeta{})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if gotPath != "/v2/videos/generations" {
		t.Errorf("path=%q", gotPath)
	}
	if out.TaskID != "task-abc" || out.RequestID != "req-1" {
		t.Errorf("out=%+v", out)
	}
	if body["model"] != "minimax-video-h3" || body["prompt"] != "一只猫在雪地里奔跑" {
		t.Errorf("body=%v", body)
	}
	if body["image_url"] != "https://cdn.example/first.png" {
		t.Errorf("image_url=%v", body["image_url"])
	}
	if body["watermark"] != false {
		t.Errorf("watermark=%v want false（网关默认无水印）", body["watermark"])
	}
	refs, _ := body["reference_images"].([]any)
	if len(refs) != 2 {
		t.Fatalf("reference_images=%v", body["reference_images"])
	}
	if first, _ := refs[0].(map[string]any); first["image_url"] != "https://cdn.example/ref1.png" {
		t.Errorf("ref0=%v", refs[0])
	}
	extra, _ := body["extra_parameters"].(map[string]any)
	if extra["resolution"] != "768P" || extra["aspect_ratio"] != "16:9" || extra["enable_audio"] != true {
		t.Errorf("extra_parameters=%v", extra)
	}
}

// TestQueryVideoCompleted 查询解析 status/data[].url/usage.credit。
func TestQueryVideoCompleted(t *testing.T) {
	var gotPath string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != `{"task_id":"task-abc"}` {
			t.Errorf("query body=%s", raw)
		}
		return jsonResp(200, `{"code":0,"requestId":"req-2","data":{
			"status":"completed","data":[{"url":"https://cdn.example/out.mp4"}],
			"usage":{"credit":12.5}}}`), nil
	})
	out, err := c.QueryVideo(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "task-abc", ChatMeta{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if gotPath != "/v2/videos/tasks" {
		t.Errorf("path=%q", gotPath)
	}
	if out.Status != "completed" || len(out.Videos) != 1 || out.Videos[0].URL != "https://cdn.example/out.mp4" {
		t.Errorf("out=%+v", out)
	}
	if !out.HasCredit || out.Credit != 12.5 {
		t.Errorf("credit=%v has=%v", out.Credit, out.HasCredit)
	}
}

// TestQueryVideoFailed 失败状态不带 credit。
func TestQueryVideoFailed(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"status":"failed"}}`), nil
	})
	out, err := c.QueryVideo(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "t1", ChatMeta{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if out.Status != "failed" || out.HasCredit {
		t.Errorf("out=%+v", out)
	}
}

// TestVideoBusinessError 业务 code 转 *Error（ErrClient 兜底，不罚号）。
func TestVideoBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":14401,"msg":"model not available","requestId":"r1"}`), nil
	})
	_, err := c.SubmitVideo(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"},
		VideoSubmitInput{Prompt: "x"}, ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrClient {
		t.Fatalf("err=%v", err)
	}
	if ue.Msg == "" {
		t.Error("错误信息不能为空")
	}
}

// TestNormalizeVideoInput 参数校验：无提示词且无参考素材、只给尾帧、素材超限。
func TestNormalizeVideoInput(t *testing.T) {
	if _, err := NormalizeVideoInput(VideoSubmitInput{}); err == nil {
		t.Error("空 prompt + 无图 应报错")
	}
	if _, err := NormalizeVideoInput(VideoSubmitInput{ImageURL: "https://a/1.png"}); err != nil {
		t.Errorf("图生视频允许空 prompt: %v", err)
	}
	if _, err := NormalizeVideoInput(VideoSubmitInput{LastImageURL: "https://a/2.png"}); err == nil {
		t.Error("只有尾帧应报错")
	}
	tooMany := VideoSubmitInput{Prompt: "x"}
	for i := 0; i <= VideoMaxReferenceImages; i++ {
		tooMany.ReferenceImages = append(tooMany.ReferenceImages, "https://a/i.png")
	}
	if _, err := NormalizeVideoInput(tooMany); err == nil {
		t.Error("参考图超限应报错")
	}
	in, err := NormalizeVideoInput(VideoSubmitInput{Prompt: "  x  "})
	if err != nil {
		t.Fatal(err)
	}
	if in.Model != VideoDefaultModel {
		t.Errorf("model=%q want default %q", in.Model, VideoDefaultModel)
	}
}
