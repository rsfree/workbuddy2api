package upstream

import "testing"

func TestModelTypeFromTags(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"text-to-image"}, "image"},
		{[]string{"image-to-image"}, "image"},
		{[]string{"text-to-video"}, "video"},
		{[]string{"image-to-video"}, "video"},
		{[]string{"text-to-video", "image-to-video"}, "video"},
		{[]string{"badge:限时免费"}, "chat"},
		{nil, "chat"},
	}
	for _, tc := range cases {
		if got := modelTypeFromTags(tc.tags); got != tc.want {
			t.Errorf("modelTypeFromTags(%v)=%q want %q", tc.tags, got, tc.want)
		}
	}
}

// TestModelInfoDerivesType 目录条目经 modelInfo() 后必须带上媒体类型，
// 否则 FetchModels 的 media 分流会把它当 chat。
func TestModelInfoDerivesType(t *testing.T) {
	mi := dynModelEntry{ID: "kling-video-v3", Tags: []string{"text-to-video", "image-to-video"}}.modelInfo()
	if mi.Type != "video" {
		t.Fatalf("Type=%q want video", mi.Type)
	}
	mi = dynModelEntry{ID: "hunyuan-image-alpha", Tags: []string{"text-to-image"}}.modelInfo()
	if mi.Type != "image" {
		t.Fatalf("Type=%q want image", mi.Type)
	}
}

// TestMediaModelsSnapshot 媒体目录按 realm 隔离且返回副本。
func TestMediaModelsSnapshot(t *testing.T) {
	c := &Client{}
	c.StoreMediaModels("cn", []ModelInfo{{ID: "hunyuan-image-alpha", Type: "image"}})
	c.StoreMediaModels("global", []ModelInfo{{ID: "gpt-image-x", Type: "image"}})

	cn := c.MediaModels("cn")
	if len(cn) != 1 || cn[0].ID != "hunyuan-image-alpha" {
		t.Fatalf("cn media=%+v", cn)
	}
	if got := c.MediaModels("global"); len(got) != 1 || got[0].ID != "gpt-image-x" {
		t.Fatalf("global media=%+v", got)
	}
	// 调用方改副本不能污染缓存。
	cn[0].ID = "mutated"
	if got := c.MediaModels("cn"); got[0].ID != "hunyuan-image-alpha" {
		t.Fatalf("cache mutated: %+v", got)
	}
	if got := c.MediaModels("nope"); got != nil {
		t.Fatalf("unknown realm=%+v", got)
	}
}
