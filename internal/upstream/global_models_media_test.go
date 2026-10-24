package upstream

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Global 探测的聊天与媒体目录必须分流且各自合并：媒体既不能混入 chat，
// 也不能像旧实现一样在 nonChatModel 过滤后直接消失。
func TestGlobalProbeMediaSplitAndMerge(t *testing.T) {
	in := probeResult{
		names: []string{"chat", "image", "video", "completion-x"},
		infos: []ModelInfo{
			{ID: "chat", Type: "chat", MaxTokens: 32000},
			{ID: "image", Type: "image", Tags: []string{"image-to-image"}},
			{ID: "video", Type: "video", Tags: []string{"text-to-video"}},
			{ID: "completion-x", Type: "chat", MaxTokens: 128},
		},
	}
	got := splitGlobalProbeMedia(in)
	if len(got.names) != 1 || got.names[0] != "chat" || len(got.infos) != 1 || got.infos[0].ID != "chat" {
		t.Fatalf("chat split=%+v", got)
	}
	if len(got.media) != 2 || got.media[0].ID != "image" || got.media[1].ID != "video" {
		t.Fatalf("media split=%+v", got.media)
	}

	merged := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{
		{label: "desktop", res: probeResult{media: []ModelInfo{{ID: "image", Type: "image", Credits: "primary"}}}},
		{label: "ide", res: probeResult{names: []string{"chat-2"}, media: []ModelInfo{{ID: "image", Type: "image", Credits: "secondary"}, {ID: "video", Type: "video"}}}},
	})
	if merged.err != nil || len(merged.media) != 2 {
		t.Fatalf("merged=%+v", merged)
	}
	if merged.media[0].Credits != "primary" || merged.media[1].ID != "video" {
		t.Fatalf("media merge priority=%+v", merged.media)
	}
}

// 媒体-only 是有效目录，必须写缓存并在 TTL 内复用；不能因 names 为空进入失败负缓存。
func TestFetchGlobalModelsMediaOnlyUsesSuccessCache(t *testing.T) {
	var calls atomic.Int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResp(200, `{"code":0,"data":{"models":[{"id":"minimax-video-h3","tags":["text-to-video"]}]}}`), nil
	})
	c.GlobalEnabled = true
	a := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}
	if infos := c.FetchGlobalModelInfos(a); len(infos) != 0 {
		t.Fatalf("chat infos=%+v want empty", infos)
	}
	if media := c.MediaModels("global"); len(media) != 1 || media[0].ID != "minimax-video-h3" {
		t.Fatalf("media=%+v", media)
	}
	firstCalls := calls.Load()
	if firstCalls == 0 {
		t.Fatal("expected upstream probes")
	}
	_ = c.FetchGlobalModelInfos(a)
	if got := calls.Load(); got != firstCalls {
		t.Fatalf("media-only success was not cached: calls %d -> %d", firstCalls, got)
	}
}

// 后续全量刷新中 v3 临时失败、企业端点只返回聊天时，不得用不完整的
// 空媒体列表清掉上一轮已确认的图片目录。
func TestFetchGlobalModelsPartialFailurePreservesMediaCache(t *testing.T) {
	var partial atomic.Bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if partial.Load() && strings.HasSuffix(r.URL.Path, "/v3/config") {
			return jsonResp(500, `{}`), nil
		}
		if partial.Load() {
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"chat","maxOutputTokens":32000}]}}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{"models":[{"id":"chat","maxOutputTokens":32000},{"id":"image","tags":["text-to-image"]}]}}`), nil
	})
	c.GlobalEnabled = true
	a := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}
	_ = c.FetchGlobalModelInfos(a)
	if media := c.MediaModels("global"); len(media) != 1 {
		t.Fatalf("initial media=%+v", media)
	}
	partial.Store(true)
	c.globalModels.Lock()
	c.globalModels.fetched = time.Time{}
	c.globalModels.Unlock()
	if infos := c.FetchGlobalModelInfos(a); len(infos) != 1 || infos[0].ID != "chat" {
		t.Fatalf("degraded chat infos=%+v", infos)
	}
	if media := c.MediaModels("global"); len(media) != 1 || media[0].ID != "image" {
		t.Fatalf("partial refresh cleared media cache: %+v", media)
	}
}
