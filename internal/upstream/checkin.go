// checkin.go 每日签到回执的四态分类。
//
// 为什么单独成文：签到是「一天一次、漏一天少一天积分」的幂等动作，但它的回执
// 至少有四种语义——成功 / 今天已签 / 上游排队中 / 业务失败——而旧实现只有一个
// 裸子串（"已签到"）在区分「幂等拒绝」与「成功」。这个子串同时出现在失败文案里：
//
//	「签到失败，已签到天数不足 7 天」
//
// 于是失败被判成「今天已签到」，`lastCheckinDay` 被写成今天，当天后续两轮排程
// 都不再重试，面板还显示「已签」——一次漏签被完整地藏起来。
//
// 四态的口径（对上游抓到的真实形态，见 v1.9.0 同类实现 notes）：
//   - 判定顺序固定为 pending → failed → already：越具体的语义越先判。
//     「签到失败，已签到天数不足 7 天」同时命中 failed 与 already，它必须是 failed。
//   - code=0 不等于成功：上游会把失败文案挂在成功信封里。
//   - only CheckinDone / CheckinAlready 是「今天这次签到已有结果」（Settled），
//     只有它们可以写 lastCheckinDay。
package upstream

import (
	"errors"
	"strings"
)

// CheckinOutcome 签到回执分类。
type CheckinOutcome int

const (
	// CheckinUnknown 无法判定（网络层 / 解析层错误，上游没有表态）。
	CheckinUnknown CheckinOutcome = iota
	// CheckinDone 签到成功，积分已到账。
	CheckinDone
	// CheckinAlready 今天已经签过（上游幂等拒绝重复签到）：当天视为完成。
	CheckinAlready
	// CheckinPending 上游还在处理（「请求处理中，请勿重复操作」）：当天**未**完成，
	// 下一轮排程应当重试，绝不能写 lastCheckinDay。
	CheckinPending
	// CheckinFailed 业务失败（连登天数不足、活动未开始等）。不写 lastCheckinDay：
	// 上游次日可能恢复正常，写死会让面板与调度都误判成「今天已签」。
	CheckinFailed
)

func (o CheckinOutcome) String() string {
	switch o {
	case CheckinDone:
		return "checked"
	case CheckinAlready:
		return "already"
	case CheckinPending:
		return "pending"
	case CheckinFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Settled 报告这次签到是否已经有了确定结果（成功或今天已签）。
// 只有为真时调用方才该把账号标记成「今日已签」。
func (o CheckinOutcome) Settled() bool { return o == CheckinDone || o == CheckinAlready }

// checkinPendingMarkers 上游排队 / 去重形态。它不是失败：登录高峰时段签到会返回
// 这个，隔一会儿重试就能成功，所以既不能写已签、也不能算失败。
//
// "请勿重复" 必须排在 already 之前：同一条文案里既有「请勿重复」也有「签到」，
// 但它的语义是「刚才那次还在跑」，重试才有意义。
var checkinPendingMarkers = []string{
	"请勿重复", "处理中", "正在处理", "稍后再试", "稍后重试", "请求频繁",
	"processing", "in progress", "try again later", "please retry",
}

// checkinFailedMarkers 明确的失败文案。
//
// 🔴 必须排在 already 之前：「签到失败，已签到天数不足 7 天」同时命中两边，
// 失败才是这条回执的语义。只写「不足」会误伤「余额不足」这类无关文案，
// 所以收的是「天数不足」这种带主语的形态。
var checkinFailedMarkers = []string{
	"签到失败", "签退失败", "天数不足", "不足 7 天", "不足7天", "未满足条件",
	"活动未开始", "活动已结束", "check-in failed", "checkin failed", "not eligible",
}

// checkinAlreadyMarkers 「今天已签到」的**完整短语**。
//
// 🔴 这里绝不能出现裸 "已签到"：它在失败文案里同样存在，那正是「失败被记成
// 成功」的直接原因。每条都得带上「今天 / 今日 / 已经 / 过 / 重复」这类限定词。
var checkinAlreadyMarkers = []string{
	"今天已签到", "今日已签到", "今天已经签到", "今日已经签到",
	"今天已签", "今日已签", "已签到过", "已经签到", "重复签到",
	"already checked", "already checkin", "already check-in", "already signed",
}

// ClassifyCheckin 把一次签到回执归一成四态。
//
// code 为上游业务码（0 = 成功信封，HTTP 层错误时传 0 或 StatusCode 均可）；
// payload 为业务文案或整个 data 片段——按子串匹配，JSON 形态也能直接喂。
func ClassifyCheckin(code int, payload string) CheckinOutcome {
	low := strings.ToLower(payload)
	hit := func(markers []string) bool {
		for _, m := range markers {
			if strings.Contains(low, strings.ToLower(m)) {
				return true
			}
		}
		return false
	}
	switch {
	case hit(checkinPendingMarkers):
		return CheckinPending
	case hit(checkinFailedMarkers):
		return CheckinFailed
	case hit(checkinAlreadyMarkers):
		return CheckinAlready
	case code == 0:
		// 成功信封且没有任何失败/排队迹象：这就是一次正常签到。
		return CheckinDone
	default:
		// 非 0 业务码却没命中任何词表：保守判失败——不确定「已签」时绝不能
		// 写 lastCheckinDay，否则当天再也不会重试。
		return CheckinFailed
	}
}

// CheckinOutcomeOf 从 DailyCheckin 的返回值归一四态。
// 非 *Error（网络 / 解析层）一律 CheckinUnknown：上游没表态，不能记成已签。
func CheckinOutcomeOf(outcome CheckinOutcome, err error) CheckinOutcome {
	if err == nil {
		return outcome
	}
	var ue *Error
	if !errors.As(err, &ue) {
		return CheckinUnknown
	}
	return ClassifyCheckin(ue.Code, ue.Msg)
}
