package panel

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// onboard.go 授权登录新账号「自动清空成长任务」。
//
// 触发链路：
//
//	POST /panel/api/import/cockpit?onboard=auto|1|0（缺省 **off**）
//	  → 导入成功且新账号数满足条件 → p.enqueueOnboard
//	  → 单 worker 串行：基线余额 → runAutoAll（批量接受 + 全动作表 + 达标补领）
//	    → 补一轮 accept/claim → 终值余额
//	  → GET /panel/api/onboard 查看每号 before/after/增量
//
// 为什么 auto 只认「本次恰好 1 个新账号」：授权链接登录（`./login url` → 浏览器授权
// → `./login poll`）天然是单账号导入；批量导入（csv/多文件）一次几十个号，每个号的
// 全量动作都含真实对话，自动跑会成倍消耗上游配额。用户 2026-10-24 明确：只对
// 「登录授权链接」进来的账号自动做任务，批量导入不跑。批量也要跑时显式传 onboard=1。
//
// 为什么缺省是 off 而不是 auto：导入脚本（`import_accounts.py`）为了并发**按账号拆
// 单请求**，批量 20 个号 = 20 个独立请求，服务端每次都只看到「1 个新账号」——auto
// 判据会被拆单绕过，一次批量就能把 20 个号的全量动作全跑一遍。所以必须由客户端
// 显式声明意图：授权登录的单号导入传 `onboard=auto`，批量导入传 `onboard=0`（脚本
// 已按待导入数量自动选择）。缺省 off 让任何「忘了声明」的调用都退回安全侧。
//
// 与既有链路的关系：`auto_all` 负责「把 0 进度任务做出来」，`tasksweep` 只做
// 「accept 未接受 + claim 已达标未领」。本文件把前者串成导入后的自动步骤，并额外
// 补一轮 sweep，把动作点亮但没来得及领的奖励也一起领掉（等同人工两轮 auto_all）。

// onboardMaxRecords 留痕上限（进程内存，账号数有界）。
const onboardMaxRecords = 100

// onboard 触发模式。
const (
	onboardModeAuto  = "auto"  // 仅本次恰好 1 个新账号时触发（授权链接登录的形态）
	onboardModeForce = "force" // 无论几个新账号都触发
	onboardModeOff   = "off"   // 关闭（缺省：防批量拆单请求误触发）
)

// onboardRecord 单个账号的自动清任务留痕（GET /panel/api/onboard 返回）。
type onboardRecord struct {
	UID           string `json:"uid"`
	Nickname      string `json:"nickname"`
	Realm         string `json:"realm"`
	Status        string `json:"status"` // running | done | skipped | error
	Reason        string `json:"reason,omitempty"`
	CreditsBefore int64  `json:"credits_before"`
	CreditsAfter  int64  `json:"credits_after"`
	Delta         int64  `json:"delta"`
	Claimed       int    `json:"claimed"`
	Message       string `json:"message,omitempty"`
	StartedAt     int64  `json:"started_at"`
	FinishedAt    int64  `json:"finished_at,omitempty"`
}

// onboardJob 队列项。
type onboardJob struct {
	a      *auth.Auth
	reason string
}

// onboardExecutor 跑一轮完整任务流水线，返回 (完成任务数, 基线余额, 终值余额, err)。
// 默认实现 execOnboard 走真实上游；测试可注入替身避免真实对话。
type onboardExecutor func(p *Panel, a *auth.Auth) (claimed int, before, after int64, err error)

// onboard 流水线里「不跑」的三种情形（记为 skipped，不是故障）。
var (
	errOnboardBusy       = errors.New("该账号有任务动作正在执行中，本轮跳过")
	errOnboardEnterprise = errors.New("企业版/已绑定租户会话无个人成长体系")
	errOnboardGlobal     = errors.New("global 账号无 CN 成长体系")
)

// parseOnboardMode 归一化 onboard 参数：auto 走单号判据；1/true/force 强制；
// 其余（含缺省、非法值、0/false/off）一律关闭——安全默认见文件头「为什么缺省是 off」。
func parseOnboardMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto", "default":
		return onboardModeAuto
	case "1", "true", "on", "yes", "force", "all":
		return onboardModeForce
	default:
		return onboardModeOff
	}
}

// shouldOnboard 判定本次导入是否触发自动清任务。
func shouldOnboard(mode string, newAccounts int) bool {
	if newAccounts <= 0 {
		return false
	}
	switch mode {
	case onboardModeOff:
		return false
	case onboardModeForce:
		return true
	default: // auto：授权链接登录的形态 = 本次只进 1 个新号
		return newAccounts == 1
	}
}

// enqueueOnboard 把新账号投入串行队列（首个任务到达时惰性启动 worker）。
// 全局面板只有一个 worker，多个号串行跑，避免同时开多条真实对话链把额度打满。
func (p *Panel) enqueueOnboard(list []*auth.Auth, reason string) {
	if len(list) == 0 {
		return
	}
	p.onboardOnce.Do(func() {
		p.onboardCh = make(chan onboardJob, 256)
		go p.onboardWorker()
	})
	for _, a := range list {
		p.onboardCh <- onboardJob{a: a, reason: reason}
	}
}

// onboardWorker 串行消费队列（单 goroutine = 串行，天然节流）。
func (p *Panel) onboardWorker() {
	for job := range p.onboardCh {
		p.runOnboard(job)
	}
}

// appendOnboard 追加一条留痕并返回其下标（供结束时原地更新）。
func (p *Panel) appendOnboard(rec onboardRecord) int {
	p.onboardMu.Lock()
	defer p.onboardMu.Unlock()
	p.onboardLog = append(p.onboardLog, rec)
	if over := len(p.onboardLog) - onboardMaxRecords; over > 0 {
		p.onboardLog = append([]onboardRecord(nil), p.onboardLog[over:]...)
	}
	return len(p.onboardLog) - 1
}

// runOnboard 执行单个账号的自动清任务并落留痕。
func (p *Panel) runOnboard(job onboardJob) {
	a := job.a
	idx := p.appendOnboard(onboardRecord{
		UID: a.UID, Nickname: a.Nickname, Realm: a.Realm(),
		Status: "running", Reason: job.reason, StartedAt: time.Now().Unix(),
	})
	log.Printf("panel: onboard 开始 uid=%s(%s) realm=%s reason=%s", a.UID, a.Nickname, a.Realm(), job.reason)

	run := p.onboardRun
	if run == nil {
		run = execOnboard
	}
	claimed, before, after, err := run(p, a)

	p.onboardMu.Lock()
	r := &p.onboardLog[idx]
	r.Claimed, r.CreditsBefore, r.CreditsAfter = claimed, before, after
	r.FinishedAt = time.Now().Unix()
	switch {
	case err == nil:
		r.Status = "done"
		r.Delta = after - before
	case errors.Is(err, errOnboardBusy), errors.Is(err, errOnboardEnterprise), errors.Is(err, errOnboardGlobal):
		r.Status = "skipped"
		r.Message = err.Error()
	default:
		r.Status = "error"
		r.Message = err.Error()
	}
	status, delta := r.Status, r.Delta
	p.onboardMu.Unlock()

	log.Printf("panel: onboard 结束 uid=%s status=%s credits=%d→%d (delta=%d) 完成任务=%d 备注=%v",
		a.UID, status, before, after, delta, claimed, err)
}

// execOnboard 默认流水线：基线 → 全量动作（含接受/补领）→ 补一轮 sweep → 终值。
func execOnboard(p *Panel, a *auth.Auth) (claimed int, before, after int64, err error) {
	if a.Realm() == "global" {
		return 0, 0, 0, errOnboardGlobal
	}
	if a.IsEnterpriseSession() {
		return 0, 0, 0, errOnboardEnterprise
	}

	if rm, _, uerr := p.cfg.Upstream.UserResource(a); uerr == nil {
		before = rm
	} else {
		return 0, 0, 0, fmt.Errorf("读取基线余额: %w", uerr)
	}

	// 全量任务动作（内部含批量接受 + 各任务行为事件 + 达标补领）。
	// 自己拿锁，避免与面板上手动「一键完成」并发跑两遍（每条 expert 链含真实对话）。
	if !p.tryLockAccount(a.UID) {
		return 0, before, before, errOnboardBusy
	}
	results := p.runAutoAll(a)
	p.unlockAccount(a.UID)
	claimed = countAutoDone(results)

	// 动作之外的达标任务（无自动动作的任务码）靠这一轮 sweep 接受 + 领奖。
	if res := p.sweepAccount(a, a.UID); res.claimed > 0 {
		claimed += res.claimed
	}

	if rm, _, uerr := p.cfg.Upstream.UserResource(a); uerr == nil {
		after = rm
	} else {
		after = before
	}
	return claimed, before, after, nil
}

// countAutoDone 统计流水线里真正完成的任务数（排除「(批量接受)」这类汇总行）。
func countAutoDone(results []map[string]any) int {
	n := 0
	for _, it := range results {
		code, _ := it["task_code"].(string)
		if code == "" || strings.HasPrefix(code, "(") {
			continue
		}
		if st, _ := it["status"].(string); st == "done" {
			n++
		}
	}
	return n
}

// onboardStatus 查询最近的自动清任务留痕（最新在前）。
//
//	GET /panel/api/onboard?limit=20
func (p *Panel) onboardStatus(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			limit = n
		}
	}
	p.onboardMu.Lock()
	total := len(p.onboardLog)
	start := total - limit
	if start < 0 {
		start = 0
	}
	out := append([]onboardRecord(nil), p.onboardLog[start:]...)
	p.onboardMu.Unlock()

	// 最新在前，面板直接渲染。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "total": total, "records": out})
}
