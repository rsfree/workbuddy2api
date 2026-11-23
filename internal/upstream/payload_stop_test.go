package upstream

import (
	"encoding/json"
	"testing"
)

// stop 字段归一化。
//
// 背景（2026-11-23 线上实测）：OpenAI 规范 stop 是 `string | string[] | null`，
// 上游 minimax 后端的 Go struct 是 []string。客户端发字符串时上游直接
//
//	400 code=11101 "cannot unmarshal string into Go struct field Request.stop of type []string"
//
// 24h 内 1077 次，占全部失败请求的 74%，全部集中在该后端（其他后端宽松放行）。
func TestNormalizeStop(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 期望的 stop JSON 字面量；"absent" 表示字段应被删除
	}{
		{"string → 单元素数组", `{"stop":"END"}`, `["END"]`},
		{"string 保持原值不做裁剪", `{"stop":" END "}`, `[" END "]`},
		{"空串删除字段", `{"stop":""}`, "absent"},
		{"纯空白串删除字段", `{"stop":"   "}`, "absent"},
		{"null 删除字段", `{"stop":null}`, "absent"},
		{"数组原样保留", `{"stop":["A","B"]}`, `["A","B"]`},
		{"空数组原样保留", `{"stop":[]}`, `[]`},
		{"数字删除字段", `{"stop":5}`, "absent"},
		{"对象删除字段", `{"stop":{"x":1}}`, "absent"},
		{"无 stop 字段不动", `{"temperature":0.2}`, "absent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var obj map[string]any
			if err := json.Unmarshal([]byte(c.in), &obj); err != nil {
				t.Fatal(err)
			}
			normalizeStop(obj)
			got, ok := obj["stop"]
			if c.want == "absent" {
				if ok {
					t.Fatalf("stop 应被删除，实际 %#v", got)
				}
				return
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != c.want {
				t.Fatalf("stop = %s，期望 %s", b, c.want)
			}
		})
	}
}

// 端到端：出站管线必须把 string 形态转成数组，且不影响既有强制行为。
func TestPrepareBodyNormalizesStop(t *testing.T) {
	in := []byte(`{"model":"minimax-m3.1-flash","stop":"END",` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	out := PrepareBodyOpt(in, false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	arr, ok := obj["stop"].([]any)
	if !ok {
		t.Fatalf("stop 类型 = %T，期望数组", obj["stop"])
	}
	if len(arr) != 1 || arr[0] != "END" {
		t.Fatalf("stop = %#v，期望 [\"END\"]", arr)
	}
	if obj["stream"] != true {
		t.Error("stream 未被强制")
	}
}

// 同一入参两次改写的输出必须逐字节一致：stop 归一化不能引入 map 迭代顺序抖动。
func TestPrepareBodyStopDeterministic(t *testing.T) {
	in := []byte(`{"model":"x","stop":"END","messages":[{"role":"user","content":"hi"}]}`)
	a := string(PrepareBodyOpt(in, false))
	b := string(PrepareBodyOpt(in, false))
	if a != b {
		t.Fatalf("输出非确定性:\n%s\n%s", a, b)
	}
}
