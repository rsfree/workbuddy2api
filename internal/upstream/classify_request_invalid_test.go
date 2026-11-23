package upstream

import "testing"

// 11151 / 11133 的请求级分类。
//
// 背景（2026-11-23 线上实测，24h）：11151「a message has empty content」429 次、
// 11133「Invalid request parameters + extError 1214」218 次尝试级失败。两者此前
// 无 marker，落通用 4xx 兜底 = ErrClient（只换号不罚），导致同一个坏 body 被换号
// 放大 MaxRotate(3) 倍、最终以 503 返回（语义误导为「服务过载」），并给 3 个健康
// 账号各喂一次 NoteFailures 连败计数。
//
// 它们与 11101（Unmarshal chat params failed）同属「同一个 body 换任何账号结果
// 不变」的确定性请求错误，因此必须归 ErrBadParams：不罚号、不轮转、400 透传原文。
func TestClassifyRequestInvalidAsBadParams(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{
			name:   "11151 空 content",
			status: 400,
			body: `{"code":11151,"msg":"a message has empty content, please check the conversation history and retry",` +
				`"retry_policy":{"retry":false},"extError":{"code":"400"}}`,
			want: ErrBadParams,
		},
		{
			name:   "11151 code 带 JSON 空白",
			status: 400,
			body:   `{"code": 11151,"msg":"empty content"}`,
			want:   ErrBadParams,
		},
		{
			name:   "11151 无 code，仅语义唯一文案",
			status: 400,
			body:   `{"msg":"a message has empty content, please check the conversation history"}`,
			want:   ErrBadParams,
		},
		{
			name:   "11133 参数被模型提供方拒绝仍走通用 4xx",
			status: 400,
			body: `{"code":11133,"msg":"Invalid request parameters",` +
				`"extError":{"code":"1214","message":"the request parameters were rejected by the model provider"}}`,
			want: ErrClient,
		},
		{
			name:   "11133 字符串 code 形态同样不并入",
			status: 400,
			body:   `{"code":"11133","msg":"Invalid request parameters"}`,
			want:   ErrClient,
		},
		// 这条是 11133 不能一刀切的正面证据：同一业务码也用于「模型不支持图片」，
		// 换域/换号可能拿到支持图片的后端。把它并入 ErrBadParams 会把本可自愈的
		// 请求硬失败成 400（既有 TestClassify 亦以此为守卫）。
		{
			name:   "11133 模型不支持图片必须保留换号机会",
			status: 400,
			body:   `{"code": 11133, "msg":"model does not support image"}`,
			want:   ErrClient,
		},
		// 反向守卫：11133 的 msg 单独出现时过于通用，不得凭文案改判——否则一个
		// 真实的账号级 4xx 会被硬失败成 400，客户端失去换号自愈的机会。
		{
			name:   "单独 Invalid request parameters 文案不误判",
			status: 400,
			body:   `{"msg":"Invalid request parameters"}`,
			want:   ErrClient,
		},
		// 状态码优先：429 已在上层按限流语义判定，不能被请求级 marker 抢走。
		{
			name:   "429 携带 11151 仍是限流",
			status: 429,
			body:   `{"code":11151,"msg":"a message has empty content"}`,
			want:   ErrSoftRate,
		},
		// 非 4xx 不参与请求级判定。
		{
			name:   "200 携带 11151 不命中",
			status: 200,
			body:   `{"code":11151,"msg":"a message has empty content"}`,
			want:   ErrNone,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Fatalf("Classify(%d, %q) = %v，期望 %v", c.status, c.body, got, c.want)
			}
		})
	}
}
