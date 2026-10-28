package upstream

// 临时只读探针：枚举 growth/billing/portal 域下可获取积分的能力。
// 全部为 GET（不产生进度、不消耗额度、不触发生成）。跑完即删。

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestZZProbeCredits(t *testing.T) {
	auths, err := auth.LoadDir("/tmp/probe_auths")
	if err != nil || len(auths) == 0 {
		t.Fatalf("load auth: %v n=%d", err, len(auths))
	}
	a := auths[0]
	c := &Client{
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		ChatBaseCN:   "https://copilot.tencent.com",
		BillingBaseCN: "https://www.codebuddy.cn",
		WebBaseCN:    "https://www.workbuddy.cn",
	}

	out := "/tmp/probe_out"
	_ = os.MkdirAll(out, 0o755)

	type probe struct {
		name string
		kind string // growth / growthmp / billing / billingv2 / web
		path string
	}
	probes := []probe{
		// 已知：任务与成长
		{"01_tasks_pc", "growth", "/activity/growth/tasks"},
		{"02_tasks_mp", "growthmp", "/v2/activity/growth/tasks"},
		{"03_streak", "growth", "/activity/growth/streak"},
		{"04_heatmap", "growth", "/activity/growth/heatmap"},
		{"05_lottery", "growth", "/activity/growth/lottery/summary"},
		{"06_buddy", "growth", "/activity/growth/buddy/info"},
		{"07_travel", "growth", "/activity/growth/buddy/travel/status"},
		// 已知：计费域
		{"08_resource", "billingv2", "/v2/billing/meter/get-user-resource"},
		{"09_compensation", "billing", "/billing/meter/compensation-status"},
		{"10_checkin_get", "billingv2", "/v2/billing/meter/daily-checkin"},
		// 已知：活动域
		{"11_school", "web", "/portal/activity/school"},
		{"12_school_vouchers", "web", "/portal/activity/school/vouchers"},
		// 探测候选：growth 子域新玩法
		{"20_home", "growth", "/activity/growth/home"},
		{"21_summary", "growth", "/activity/growth/summary"},
		{"22_season", "growth", "/activity/growth/season"},
		{"23_missions", "growth", "/activity/growth/missions"},
		{"24_rewards", "growth", "/activity/growth/rewards"},
		{"25_mall", "growth", "/activity/growth/mall"},
		{"26_invite", "growth", "/activity/growth/invite"},
		{"27_records", "growth", "/activity/growth/records"},
		{"28_makeup", "growth", "/activity/growth/makeup-cards"},
		{"29_signin", "growth", "/activity/growth/sign-in"},
		{"30_checkin", "growth", "/activity/growth/checkin"},
		{"31_daily", "growth", "/activity/growth/daily"},
		{"32_tasklist_alt", "growth", "/activity/growth/task/list"},
		{"33_credit_log", "growth", "/activity/growth/credit/records"},
		{"34_points", "growth", "/activity/growth/points"},
		{"35_energy", "growth", "/activity/growth/energy"},
		{"36_exchange", "growth", "/activity/growth/exchange"},
		{"37_gifts", "growth", "/activity/growth/gifts"},
		// 探测候选：计费域
		{"40_credit_records", "billing", "/billing/meter/credit-records"},
		{"41_gift", "billing", "/billing/meter/gift"},
		{"42_ide_trial", "billing", "/billing/ide/trial"},
		{"43_daily_checkin_v1", "billing", "/billing/meter/daily-checkin"},
		{"44_resource_v1", "billing", "/billing/meter/get-user-resource"},
		{"45_usage", "billingv2", "/v2/billing/meter/get-user-usage"},
		// 探测候选：活动/其他
		{"50_activity_root", "web", "/portal/activity"},
		{"51_invite", "web", "/portal/activity/invite"},
		{"52_console_credit", "web", "/console/credit"},
	}

	for _, p := range probes {
		var raw []byte
		var err error
		switch p.kind {
		case "growth":
			raw, err = c.growthJSON(a, http.MethodGet, p.path, nil)
		case "growthmp":
			raw, err = c.growthJSONMP(a, http.MethodGet, p.path, nil)
		case "billing", "billingv2":
			raw, err = c.billingJSON(a, http.MethodGet, p.path, nil)
		case "web":
			var req *http.Request
			req, err = http.NewRequest(http.MethodGet, c.webBase(a)+p.path, nil)
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
				req.Header.Set("Accept", "application/json")
				req.Header.Set("x-client-platform", "web")
				raw, err = c.doJSON(a, req)
			}
		}
		status := "OK"
		body := string(raw)
		if err != nil {
			status = "ERR " + err.Error()
			if len(status) > 240 {
				status = status[:240]
			}
		}
		if len(body) > 6000 {
			body = body[:6000] + "...(truncated)"
		}
		_ = os.WriteFile(filepath.Join(out, p.name+".json"), []byte(body), 0o644)
		fmt.Printf("[%s] %-22s %s :: %s\n", status, p.name, p.path, strings.ReplaceAll(strings.TrimSpace(body), "\n", " ")[:min(160, len(strings.TrimSpace(body)))])
	}
}
