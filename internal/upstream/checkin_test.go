// checkin_test.go 钉住签到回执的四态分类，尤其是那个把失败读成成功的形态：
// 「签到失败，已签到天数不足 7 天」里含裸子串 "已签到"，旧实现据此判成
// 「今天已签到」并写 lastCheckinDay —— 当天后续排程不再重试，一次漏签被藏起来。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestClassifyCheckinTable(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		payload string
		want    CheckinOutcome
	}{
		// —— 成功 ——
		{"成功信封空 data", 0, `{}`, CheckinDone},
		{"成功信封带 success", 0, `{"msg":"success"}`, CheckinDone},

		// —— 幂等已签（必须是完整短语） ——
		{"今日已签到 14001", 14001, `{"msg":"今日已签到"}`, CheckinAlready},
		{"今天已签到 10001", 10001, `{"msg":"今天已签到"}`, CheckinAlready},
		{"已经签到过", 10001, `{"msg":"您今天已经签到过了"}`, CheckinAlready},

		// —— 失败：这里必须拦住的正是旧实现的误判面 ——
		{"天数不足含已签到三字", 10001, `{"msg":"签到失败，已签到天数不足 7 天"}`, CheckinFailed},
		{"天数不足 code=0", 0, `{"msg":"签到失败，已签到天数不足7天"}`, CheckinFailed},
		{"活动未开始", 10001, `{"msg":"活动未开始"}`, CheckinFailed},
		{"未知业务码无文案", 99999, `{"msg":"something went wrong"}`, CheckinFailed},

		// —— 排队中：既不是成功也不是失败，下一轮要重试 ——
		{"请勿重复操作", 10001, `{"msg":"请求处理中，请勿重复操作"}`, CheckinPending},
		{"processing", 10001, `{"msg":"checkin is processing"}`, CheckinPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyCheckin(tc.code, tc.payload); got != tc.want {
				t.Fatalf("ClassifyCheckin(%d, %q)=%s want %s", tc.code, tc.payload, got, tc.want)
			}
		})
	}
}

// TestCheckinOutcomeSettled 只有「成功 / 已签」可以写 lastCheckinDay。
func TestCheckinOutcomeSettled(t *testing.T) {
	settled := map[CheckinOutcome]bool{
		CheckinDone: true, CheckinAlready: true,
		CheckinPending: false, CheckinFailed: false, CheckinUnknown: false,
	}
	for o, want := range settled {
		if got := o.Settled(); got != want {
			t.Errorf("%s.Settled()=%v want %v", o, got, want)
		}
	}
}

// TestCheckinOutcomeOfNetworkError 网络层错误不能被当成「已签」：上游没表态，
// 停机补签遇到抖动会误记，账号当天实际未签到却被判正常。
func TestCheckinOutcomeOfNetworkError(t *testing.T) {
	if got := CheckinOutcomeOf(CheckinUnknown, errors.New("dial tcp: i/o timeout")); got != CheckinUnknown {
		t.Fatalf("network err → %s want unknown", got)
	}
	ue := &Error{Kind: ErrClient, Status: 200, Code: 10001, Msg: `code=10001 msg=签到失败，已签到天数不足 7 天`}
	if got := CheckinOutcomeOf(CheckinUnknown, ue); got != CheckinFailed {
		t.Fatalf("业务失败 → %s want failed", got)
	}
}

// TestDailyCheckinFailureWithCodeZero 端到端：HTTP 200 + code 0 + 失败文案。
//
// 这是最危险的一条——err 为 nil，调用方会走「签到成功」分支。now 只有在
// 分类层识别出 data 里的失败文案，才能把它拦下来。
func TestDailyCheckinFailureWithCodeZero(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":0,"data":{"msg":"签到失败，已签到天数不足 7 天"}}`), nil
	})
	outcome, err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("code=0 不该返回 err（上游确实是 200），got %v", err)
	}
	if outcome != CheckinFailed {
		t.Fatalf("outcome=%s want failed —— code=0 的成功信封里挂着失败文案", outcome)
	}
	if outcome.Settled() {
		t.Error("失败回执不能标记今日已签")
	}
}

// TestDailyCheckinPendingNotSettled 排队中必须留白，让下一轮排程重试。
func TestDailyCheckinPendingNotSettled(t *testing.T) {
	shortBillingRetry(t)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10001,"msg":"请求处理中，请勿重复操作"}`), nil
	})
	outcome, err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("排队中是业务错误，应返回 err（上层据此不写已签）")
	}
	if outcome != CheckinPending {
		t.Fatalf("outcome=%s want pending", outcome)
	}
	if outcome.Settled() {
		t.Error("排队中不能标记今日已签")
	}
}
