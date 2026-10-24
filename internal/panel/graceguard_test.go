// graceguard_test.go 手动任务入口的静默期闸门契约测试。
//
// 守的是「手动不等手放开」这条：单任务/一键完成与自动链路跑同一套行为动作，
// 默认必须同口径拦下；force=1 是唯一的显式越权口，且越权不改变判定口径
// （TaskMuted 的两个例外 —— mtime 推断来源、积分清零 —— 必须原样继承）。
package panel

import (
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
)

func graceTestStatus() pool.Status {
	return pool.Status{
		UID:              "uid-grace-1",
		Nickname:         "新号甲",
		Credits:          2100,
		CreditsTotal:     2100,
		ImportedAt:       time.Now().Add(-6 * time.Hour),
		ImportedAtSource: "import",
	}
}

func TestManualGraceBlock(t *testing.T) {
	p := &Panel{cfg: Config{Scheduler: scheduler.New(scheduler.Config{NewAccountGraceDays: 3})}}
	plain := httptest.NewRequest("POST", "/panel/api/x", nil)
	forced := httptest.NewRequest("POST", "/panel/api/x?force=1", nil)

	t.Run("静默期内默认拦下并给原因", func(t *testing.T) {
		blocked, why := p.manualGraceBlock(plain, graceTestStatus(), "growth-manual")
		if !blocked {
			t.Fatal("新号静默期内手动任务必须被拦")
		}
		if why == "" {
			t.Fatal("拦下时必须给出原因（到期时间/第几天），否则前端只能显示无信息的 409")
		}
	})

	t.Run("force=1 显式越过", func(t *testing.T) {
		if blocked, _ := p.manualGraceBlock(forced, graceTestStatus(), "growth-manual"); blocked {
			t.Fatal("force=1 应放行")
		}
	})

	t.Run("force 取值容错", func(t *testing.T) {
		for _, v := range []string{"true", "TRUE", "yes", " 1 "} {
			// 带空格的取值必须编码进 query，否则 NewRequest 直接 panic（非法请求行）。
			r := httptest.NewRequest("POST", "/x?force="+url.QueryEscape(v), nil)
			if !forceParam(r) {
				t.Fatalf("force=%q 应被识别为越过", v)
			}
			if blocked, _ := p.manualGraceBlock(r, graceTestStatus(), "growth-manual"); blocked {
				t.Fatalf("force=%q 应放行", v)
			}
		}
		for _, v := range []string{"", "0", "false", "no", "2"} {
			r := httptest.NewRequest("POST", "/x?force="+v, nil)
			if forceParam(r) {
				t.Fatalf("force=%q 不应被识别为越过", v)
			}
		}
	})

	t.Run("过了静默期不拦", func(t *testing.T) {
		st := graceTestStatus()
		st.ImportedAt = time.Now().Add(-4 * 24 * time.Hour)
		if blocked, _ := p.manualGraceBlock(plain, st, "growth-manual"); blocked {
			t.Fatal("导入 4 天后已超出 3 天静默期，不应再拦")
		}
	})

	t.Run("mtime 推断来源不拦", func(t *testing.T) {
		// 搬机器/重写文件会让 mtime 集体漂移成"刚刚导入"，拿它判新号会把整池老号
		// 误静默三天 —— 手动入口必须继承同一个例外，否则运维点一次就 409。
		st := graceTestStatus()
		st.ImportedAtSource = "mtime"
		if blocked, _ := p.manualGraceBlock(plain, st, "growth-manual"); blocked {
			t.Fatal("来源不可信时不应拦")
		}
	})

	t.Run("积分清零放行（逃生门）", func(t *testing.T) {
		st := graceTestStatus()
		st.Credits = 0
		if blocked, _ := p.manualGraceBlock(plain, st, "growth-manual"); blocked {
			t.Fatal("credits=0 时静默期应放行，让号自己回血")
		}
	})

	t.Run("无调度器时不拦", func(t *testing.T) {
		bare := &Panel{}
		if blocked, _ := bare.manualGraceBlock(plain, graceTestStatus(), "growth-manual"); blocked {
			t.Fatal("没有调度器（裸用/测试路径）时不该有静默期语义")
		}
	})
}
