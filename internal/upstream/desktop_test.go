package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReportDesktopEventFingerprint 断言桌面指纹上报：走 chatBase、UA 为桌面形状、
// 事件数组自动注入 workbuddy-desktop 公共指纹（覆盖同名业务键）。
func TestReportDesktopEventFingerprint(t *testing.T) {
	var got []map[string]any
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		gotUA = r.Header.Get("User-Agent")
		if r.Header.Get("X-User-Id") != "u-dt" {
			t.Errorf("X-User-Id=%q want u-dt", r.Header.Get("X-User-Id"))
		}
		if r.Header.Get("X-Product") != "SaaS" {
			t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("body must be array: %v", err)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u-dt", Nickname: "RenJie"},
		DesktopEvent{"eventCode": "agent_task_created", "mode": "craft"})
	if err != nil {
		t.Fatalf("report desktop: %v", err)
	}
	if gotUA != desktopUA {
		t.Errorf("UA=%q want %q", gotUA, desktopUA)
	}
	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}
	ev := got[0]
	if ev["extName"] != "workbuddy-desktop" {
		t.Errorf("extName=%v want workbuddy-desktop", ev["extName"])
	}
	if ev["ideType"] != "WorkBuddy" || ev["ideName"] != "WorkBuddy" {
		t.Errorf("ideType/ideName=%v/%v want WorkBuddy", ev["ideType"], ev["ideName"])
	}
	if ev["userId"] != "u-dt" {
		t.Errorf("userId=%v want u-dt", ev["userId"])
	}
	if ev["userNickname"] != "RenJie" {
		t.Errorf("userNickname=%v want RenJie", ev["userNickname"])
	}
	if ev["mode"] != "craft" {
		t.Errorf("业务字段 mode=%v 被指纹覆盖或缺失", ev["mode"])
	}
	// machineId 应为 uid 稳定派生（36 hex）。
	if len(ev["machineId"].(string)) != 36 {
		t.Errorf("machineId 长度=%d want 36", len(ev["machineId"].(string)))
	}
}

// TestDesktopChatSequenceShape 断言完整对话事件链的事件码顺序与关键字段。
func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")
	wantCodes := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	if len(events) != len(wantCodes) {
		t.Fatalf("events=%d want %d", len(events), len(wantCodes))
	}
	for i, want := range wantCodes {
		if events[i]["eventCode"] != want {
			t.Errorf("events[%d].eventCode=%v want %v", i, events[i]["eventCode"], want)
		}
	}
	resp := events[3]
	if resp["isSuccessful"] != true {
		t.Errorf("chat_message_response.isSuccessful=%v want true（RichMeow 判据）", resp["isSuccessful"])
	}
	if resp["finishReason"] != "stop" {
		t.Errorf("finishReason=%v want stop", resp["finishReason"])
	}
	if resp["conversationId"] != "conv-1" {
		t.Errorf("conversationId=%v want conv-1", resp["conversationId"])
	}
}

// TestDesktopBuddyAppSequenceShape 断言 buddyapp 五连事件形状。
//
// isFirstPage 的**类型**是硬契约：上游 2026-11-10 起按 schema 逐条校验 /v2/report
// 的 events，字符串 "1" 会被判格式不合法单独丢弃，整批回 400 code=10001
// "partial success: kept=4 dropped=1, invalidIndices:[2]" —— 现象是 Buddy_App 恒不点亮。
// 客户端源码里该字段由 toBuddyAppFirstEnter(= e => !e) 产出，即 bool。
func TestDesktopBuddyAppSequenceShape(t *testing.T) {
	events := DesktopBuddyAppSequence("cb_x", "企鹅教师助手")
	wantCodes := []string{
		"buddyapp_discover_click", "buddyapp_show", "buddyapp_enter_click",
		"buddyapp_auth_confirm_click", "buddyapp_bindaccount_skip_click",
	}
	if len(events) != len(wantCodes) {
		t.Fatalf("events=%d want %d", len(events), len(wantCodes))
	}
	for i, want := range wantCodes {
		if events[i]["eventCode"] != want {
			t.Errorf("events[%d].eventCode=%v want %v", i, events[i]["eventCode"], want)
		}
	}
	enter := events[2]
	if v, ok := enter["isFirstPage"].(bool); !ok {
		t.Fatalf("buddyapp_enter_click.isFirstPage=%#v want bool（字符串会被上游 format validation 丢弃）", enter["isFirstPage"])
	} else if v != true {
		t.Errorf("isFirstPage=%v want true", v)
	}
	if _, ok := enter["position"].(int); !ok {
		t.Errorf("position=%#v want int", enter["position"])
	}
}

// TestSetAppearanceTheme 断言主题设置端点形状（点亮 Hp_Appearance 的 API）。
func TestSetAppearanceTheme(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/user-asset/appearance/set" {
			t.Errorf("path=%s want /v2/user-asset/appearance/set", r.URL.Path)
		}
		if r.Header.Get("User-Agent") != desktopUA {
			t.Errorf("UA=%q want desktop UA", r.Header.Get("User-Agent"))
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.SetAppearanceTheme(&auth.Auth{AccessToken: "at", UID: "u1"}, "theme-tkmw7j"); err != nil {
		t.Fatalf("set theme: %v", err)
	}
	if got["kind"] != "theme" || got["resource_key"] != "theme-tkmw7j" {
		t.Errorf("body=%v want kind=theme resource_key=theme-tkmw7j", got)
	}
}

// TestMarketExpertSearchUsesKeyword 定向专家不能再靠推荐分页猜测：上游 list
// 端点的 keyword 会把数万条市场记录过滤到目标，请求必须带该字段。
func TestMarketExpertSearchUsesKeyword(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/portal/operation-platform/market/expert/list" {
			t.Errorf("path=%s want expert list", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{
				"total_count": 1,
				"experts": []map[string]any{{
					"expert_id": "ex_xQHHD1u5Jkua", "expert_type": "agent",
					"display_name_zh": "无畏契约拼豆专家", "profession_zh": "无畏契约拼豆专家",
				}},
			},
		})
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	experts, err := c.MarketExpertSearch(&auth.Auth{AccessToken: "at", UID: "u1"}, "无畏契约拼豆专家")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got["keyword"] != "无畏契约拼豆专家" {
		t.Errorf("keyword=%v want 无畏契约拼豆专家", got["keyword"])
	}
	if got["page"] != float64(1) || got["page_size"] != float64(50) {
		t.Errorf("page/page_size=%v/%v want 1/50", got["page"], got["page_size"])
	}
	if len(experts) != 1 || experts[0].ExpertID != "ex_xQHHD1u5Jkua" {
		t.Errorf("experts=%+v want Valorant expert", experts)
	}
}

// TestReportDesktopEventPartialSuccess 断言 events 批上报的「部分成功」语义：
// 上游逐条 schema 校验后 kept>0、整批仍回 400 code=10001 —— 数据已入库，
// 不许当失败（否则补给白退避 45m 并重复补报）。
func TestReportDesktopEventPartialSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":10001,"msg":"partial success: kept=4 events, dropped=1 events by format validation","requestId":"r1","data":{"dropped":1,"invalidIndices":[2],"kept":4}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u-dt"},
		DesktopEvent{"eventCode": "a"}, DesktopEvent{"eventCode": "b"})
	if err != nil {
		t.Fatalf("kept>0 的部分成功应放行，得到错误: %v", err)
	}
}

// TestReportDesktopEventAllDroppedIsError 断言整批被拒（kept=0）仍是错误：
// 一条都没入库，调用方必须能重试，不能被「partial success」文案一起放过。
func TestReportDesktopEventAllDroppedIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":10001,"msg":"partial success: kept=0 events, dropped=2 events by format validation","data":{"dropped":2,"invalidIndices":[0,1],"kept":0}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u-dt"},
		DesktopEvent{"eventCode": "a"}, DesktopEvent{"eventCode": "b"})
	if err == nil {
		t.Fatal("kept=0 必须返回错误")
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrClient {
		t.Fatalf("err=%v want *Error(ErrClient)", err)
	}
}
