package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestStickyBusyOnlyOnInFlightFull 锁住「临时繁忙」与「持久不可用」的区分：
// 前者必须保留绑定（StickyBusy），后者才允许 handler 解绑（StickyUnusable）。
//
// 这条区分是缓存命中率的关键：在途满只是并发兄弟请求占着名额，把绑定解掉等于
// 把整段会话永久搬到冷账号，上游账号级前缀缓存从此重算（线上实测 99.9% → 0%）。
func TestStickyBusyOnlyOnInFlightFull(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky"})
	p.SetMaxInFlight(1)

	// 占满唯一名额，模拟并发的兄弟请求正在跑。
	if !p.Acquire("sticky") {
		t.Fatal("首个在途名额应能占用")
	}

	a, why := p.PickStickyForModel("sticky", "glm-5.3-flash")
	if a != nil {
		t.Fatalf("在途满时不应返回账号，got %s", a.UID)
	}
	if why != StickyBusy {
		t.Fatalf("在途满必须归类为 StickyBusy（保留绑定），got %v", why)
	}

	// 释放后同一绑定立刻可用 —— 证明绑定没有被上面的判定动过。
	p.Release("sticky")
	a, why = p.PickStickyForModel("sticky", "glm-5.3-flash")
	if a == nil || a.UID != "sticky" || why != StickyPicked {
		t.Fatalf("释放后粘性号应恢复可用，got acct=%v why=%v", a, why)
	}
	p.Release("sticky")
}

// TestStickyUnusableOnCooldown 冷却属于持久不可用：必须解绑，否则会话会一直
// 撞同一个冷却号，`PickByUID` 每轮白跑。
func TestStickyUnusableOnCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky"})
	p.SetMaxInFlight(4)
	p.Cooldown("sticky", CoolSoft, time.Hour, "test")

	a, why := p.PickStickyForModel("sticky", "glm-5.3-flash")
	if a != nil {
		t.Fatalf("冷却中的粘性号不应出票，got %s", a.UID)
	}
	if why != StickyUnusable {
		t.Fatalf("冷却必须归类为 StickyUnusable（允许解绑），got %v", why)
	}
}

// TestStickyUnusableOnMissingUID 绑定号已从池里移除（账号被删）时同样归为持久不可用，
// 否则绑定会永久卡在一个不存在的号上。
func TestStickyUnusableOnMissingUID(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "other"})
	if _, why := p.PickStickyForModel("ghost", "glm-5.3-flash"); why != StickyUnusable {
		t.Fatalf("不存在的 uid 必须归类为 StickyUnusable，got %v", why)
	}
}

// TestPickByUIDForModelCompatStaysNilOnBusy 兼容包装层口径不变：不论成因，
// 拿不到号都返回 nil（历史调用方与既有测试依赖这一语义）。
func TestPickByUIDForModelCompatStaysNilOnBusy(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky"})
	p.SetMaxInFlight(1)
	p.Acquire("sticky")
	defer p.Release("sticky")

	if a := p.PickByUIDForModel("sticky", "glm-5.3-flash"); a != nil {
		t.Fatalf("兼容层在途满时应返回 nil，got %s", a.UID)
	}
}
