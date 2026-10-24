package panel

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 面板目录必须把已支持的媒体模型与独立工具暴露成可调试条目，
// 且端点不能被统一误写成 /v1/chat/completions。
func TestPanelCatalogMediaAndToolEntries(t *testing.T) {
	chat := panelModelEntry("cn", upstream.ModelInfo{ID: "kimi-k3-1"}, nil, "", http.DefaultClient)
	if chat["type"] != "chat" || chat["endpoint"] != "/v1/chat/completions" {
		t.Fatalf("chat entry=%+v", chat)
	}

	out := appendPanelMediaModels(nil, "cn", []upstream.ModelInfo{
		{ID: "hunyuan-image-alpha", Type: "image"},
		{ID: "minimax-video-h3", Type: "video"},
		{ID: "glm-5.3", Type: "chat"},
		{ID: "broken"},
	}, http.DefaultClient)
	if len(out) != 2 {
		t.Fatalf("media entries=%d want 2", len(out))
	}
	if out[0]["id"] != "cn:hunyuan-image-alpha" || out[0]["type"] != "image" ||
		out[0]["endpoint"] != "/v1/images/generations" || out[0]["edits_endpoint"] != "/v1/images/edits" {
		t.Fatalf("image entry=%+v", out[0])
	}
	if out[1]["id"] != "cn:minimax-video-h3" || out[1]["type"] != "video" ||
		out[1]["endpoint"] != "/v1/videos/generations" || out[1]["poll_endpoint"] != "/v1/videos/{id}" {
		t.Fatalf("video entry=%+v", out[1])
	}

	tools := panelToolEntries()
	if len(tools) != 2 || tools[0]["type"] != "search" || tools[0]["endpoint"] != "/v1/search/messages" ||
		tools[1]["type"] != "fetch" || tools[1]["endpoint"] != "/v1/fetch" {
		t.Fatalf("tool entries=%+v", tools)
	}
}
