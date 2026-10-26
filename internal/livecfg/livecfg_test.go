package livecfg

import "testing"

// TestNormalizeHiddenModels 归一化语义：
//   - nil/空/全空白 → nil（不隐藏）
//   - 裸名 → 两域都藏（"cn:x" + "global:x"）
//   - "cn:"/"global:" 前缀 → 只藏该域
//   - 非 cn/global 前缀（如 "xx:yy"）视为裸名整体（与 resolveModel 对称：
//     前缀不合法时 realm=cn、bare=原串，网关侧检查 "cn:xx:yy" 命中）
func TestNormalizeHiddenModels(t *testing.T) {
	if got := NormalizeHiddenModels(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{}); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	if got := NormalizeHiddenModels([]string{"  ", ""}); got != nil {
		t.Errorf("all blank → %v, want nil", got)
	}

	got := NormalizeHiddenModels([]string{"deepseek-v3"})
	for _, want := range []string{"cn:deepseek-v3", "global:deepseek-v3"} {
		if _, ok := got[want]; !ok {
			t.Errorf("bare name: missing %q in %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("bare name: got %d entries %v, want 2", len(got), got)
	}

	got = NormalizeHiddenModels([]string{"cn:pro-model"})
	if _, ok := got["cn:pro-model"]; !ok {
		t.Errorf("cn: prefix: missing %q in %v", "cn:pro-model", got)
	}
	if _, ok := got["global:pro-model"]; ok {
		t.Errorf("cn: prefix: %q must not hide global realm", "global:pro-model")
	}

	// 前后空白被裁掉
	got = NormalizeHiddenModels([]string{"  glm-5.2  "})
	if _, ok := got["cn:glm-5.2"]; !ok {
		t.Errorf("trimmed bare name: missing %q in %v", "cn:glm-5.2", got)
	}

	// 非法前缀视为裸名整体
	got = NormalizeHiddenModels([]string{"xx:yy"})
	for _, want := range []string{"cn:xx:yy", "global:xx:yy"} {
		if _, ok := got[want]; !ok {
			t.Errorf("odd prefix: missing %q in %v", want, got)
		}
	}
}

// TestNormalizeExtraModels 补录归一化语义：
//   - nil/空/全空白/无 id → nil
//   - 裸名 → 只落 cn（与 hidden 的两域语义不同：补录必须给客户端一个确切 id）
//   - "global:" 前缀 → 只落该域
//   - type 只认 image/video，其余（含 "chat"）落空串
//   - 同 id 去重（先到先得）
func TestNormalizeExtraModels(t *testing.T) {
	if got := NormalizeExtraModels(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	if got := NormalizeExtraModels([]ExtraModel{{ID: "  "}, {ID: ""}}); got != nil {
		t.Errorf("blank ids → %v, want nil", got)
	}

	got := NormalizeExtraModels([]ExtraModel{
		{ID: " hy-image-3.5-preview ", Type: "IMAGE", Name: " Hy 3.5 "},
		{ID: "global:hy-video", Type: "video"},
		{ID: "cn:chat-model", Type: "chat"},
		{ID: "hy-image-3.5-preview", Type: "image"}, // 重复，先到先得
	})
	if len(got) != 3 {
		t.Fatalf("got %d entries %+v, want 3", len(got), got)
	}
	if got[0].ID != "cn:hy-image-3.5-preview" || got[0].Type != "image" || got[0].Name != "Hy 3.5" {
		t.Errorf("bare name entry = %+v, want cn-prefixed image + trimmed name", got[0])
	}
	if got[1].ID != "global:hy-video" || got[1].Type != "video" {
		t.Errorf("global entry = %+v", got[1])
	}
	if got[2].ID != "cn:chat-model" || got[2].Type != "" {
		t.Errorf("chat type must collapse to empty, got %+v", got[2])
	}
}
