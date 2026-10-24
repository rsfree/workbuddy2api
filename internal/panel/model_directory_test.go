package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 聊天目录探测失败时，已缓存的媒体和独立工具仍需可见；响应同时保留 warnings，
// 不能把“部分目录失败”静默伪装成完整成功。
func TestModelsKeepsMediaAndToolsWhenChatProbeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	up.StoreMediaModels("cn", []upstream.ModelInfo{{ID: "minimax-video-h3", Type: "video"}})
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p := New(Config{Pool: pl, Upstream: up})

	req := httptest.NewRequest(http.MethodGet, "/panel/api/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Models   []map[string]any `json:"models"`
		Warnings []string         `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, m := range got.Models {
		id, _ := m["id"].(string)
		typ, _ := m["type"].(string)
		types[id] = typ
	}
	if types["cn:minimax-video-h3"] != "video" || types["workbuddy:web-search"] != "search" || types["workbuddy:web-fetch"] != "fetch" {
		t.Fatalf("models=%v", types)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("chat probe failure must be returned as warnings")
	}
}
