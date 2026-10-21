// egress_label_test.go 钉住「出口」只读透出：/status 的 egress 字段由 main 注入的
// 解析器填充，未注入时为空（omitempty，老部署行为不变），且解析器拿到的是账号
// 真实参数（uid/nickname/group）——面板要靠它和 [egress] 启动日志对账。
package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestStatusEgressLabel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "甲"})
	p.Add(&auth.Auth{UID: "u2", Nickname: "乙"})

	// 未注入：字段留空，不伪造 "direct"（否则老部署/关闭出口时会误导成"已走直连"）。
	if st, ok := p.Status("u1"); !ok || st.Egress != "" {
		t.Fatalf("未注入解析器时 egress 应为空: %+v ok=%v", st, ok)
	}

	type call struct{ uid, nick, group string }
	var calls []call
	p.SetEgressLabel(func(uid, nickname, group string) string {
		calls = append(calls, call{uid, nickname, group})
		if uid == "u1" {
			return "154.40.45.140"
		}
		return "direct"
	})

	if st, _ := p.Status("u1"); st.Egress != "154.40.45.140" {
		t.Fatalf("u1 egress = %q, want 154.40.45.140", st.Egress)
	}
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list = %d 个账号, want 2", len(list))
	}
	got := map[string]string{}
	for _, st := range list {
		got[st.UID] = st.Egress
	}
	if got["u1"] != "154.40.45.140" || got["u2"] != "direct" {
		t.Fatalf("List egress = %+v", got)
	}
	// 解析器必须拿到真实账号参数：按昵称/分组哈希的部署靠这两个字段定出口。
	if len(calls) == 0 || calls[0].uid != "u1" || calls[0].nick != "甲" {
		t.Fatalf("解析器入参不对: %+v", calls)
	}

	// 再注入 nil = 关闭透出（配置热更新/关停出口后不留陈旧值）。
	p.SetEgressLabel(nil)
	if st, _ := p.Status("u1"); st.Egress != "" {
		t.Fatalf("置空后仍输出 egress: %q", st.Egress)
	}
}
