package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestTaskProgressReached 锁定「动作执行成功 ≠ 任务达成」的判据。
//
// 背景（2026-10-26 线上实测）：新号 onboard 跑了 20 个动作，动作全部返回成功，
// 但 12 个任务回读仍是 0/x；早先无条件把动作结果标成 done，导致 onboard 报
// 「完成任务=20」而实际到账只有 8 个任务。判据必须只看上游进度。
func TestTaskProgressReached(t *testing.T) {
	cases := []struct {
		name string
		task *upstream.Task
		want bool
	}{
		{"回读失败(nil)判未达成以触发重试", nil, false},
		{"已领取", &upstream.Task{Claimed: true, Target: 1, Current: 0}, true},
		{"可领取", &upstream.Task{Claimable: true, Target: 5, Current: 5}, true},
		{"进度刚好达标", &upstream.Task{Target: 5, Current: 5}, true},
		{"进度差一次", &upstream.Task{Target: 5, Current: 4}, false},
		{"零进度", &upstream.Task{Target: 1, Current: 0}, false},
		{"target<=0 占位任务不阻塞", &upstream.Task{Target: 0, Current: 0}, true},
	}
	for _, c := range cases {
		if got := taskProgressReached(c.task); got != c.want {
			t.Fatalf("%s: taskProgressReached=%v, want %v", c.name, got, c.want)
		}
	}
}
