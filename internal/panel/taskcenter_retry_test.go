// taskcenter_retry_test.go 队列「限流/抖动重试」的判定契约。
//
// 关键区分：瞬时类（429 软限流 / 上游 5xx / 网关 403 WAF）值得退避重试，
// 而「任务做不动」类（余额不足、参数不合法、会话失效）重试只会拖长队列。
package panel

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestUpstreamTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"soft_rate", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429}, true},
		{"server", &upstream.Error{Kind: upstream.ErrServer, Status: 502}, true},
		{"upstream_busy", &upstream.Error{Kind: upstream.ErrUpstreamBusy}, true},
		{"not_found", &upstream.Error{Kind: upstream.ErrNotFound, Status: 404}, true},
		{"hard_credit", &upstream.Error{Kind: upstream.ErrHardCredit, Status: 402}, false},
		{"bad_params", &upstream.Error{Kind: upstream.ErrBadParams, Status: 400}, false},
		{"session_dead", &upstream.Error{Kind: upstream.ErrSessionDead, Status: 401}, false},
		{"wrapped soft_rate", fmt.Errorf("list: %w", &upstream.Error{Kind: upstream.ErrSoftRate}), true},
		{"plain error", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := upstreamTransient(c.err); got != c.want {
			t.Errorf("%s: upstreamTransient = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestQueueRetryBackoff(t *testing.T) {
	// 上游明示 Retry-After：听它的。
	if got := queueRetryBackoff(&upstream.Error{Kind: upstream.ErrSoftRate, RetryAfter: 7 * time.Second}, 1); got != 7*time.Second {
		t.Errorf("RetryAfter=7s → %v, want 7s", got)
	}
	// 夸张的 header 要钳到上限，别把队列挂住。
	if got := queueRetryBackoff(&upstream.Error{Kind: upstream.ErrSoftRate, RetryAfter: time.Hour}, 1); got != queueRetryMaxGap {
		t.Errorf("RetryAfter=1h → %v, want %v（钳到上限）", got, queueRetryMaxGap)
	}
	// 无 header：20s 起翻倍。
	if got := queueRetryBackoff(&upstream.Error{Kind: upstream.ErrSoftRate}, 1); got != queueRetryBaseGap {
		t.Errorf("attempt 1 → %v, want %v", got, queueRetryBaseGap)
	}
	if got := queueRetryBackoff(&upstream.Error{Kind: upstream.ErrSoftRate}, 2); got != 2*queueRetryBaseGap {
		t.Errorf("attempt 2 → %v, want %v", got, 2*queueRetryBaseGap)
	}
	// 非上游错误：按基准退避（调用方只在 transient 时才调，这里只守不炸）。
	if got := queueRetryBackoff(errors.New("x"), 1); got != queueRetryBaseGap {
		t.Errorf("plain err → %v, want %v", got, queueRetryBaseGap)
	}
}
