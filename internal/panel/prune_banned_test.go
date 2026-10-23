package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

const bannedReason = "account restricted by upstream (11140 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction"

// pruneEnv 造一个「2 个上游封禁号 + 1 个人工禁用 + 1 个 14017 + 1 个健康号」的最小池，
// 每个账号都有真实落盘的 auth 文件（剔除要归档文件，必须走文件系统）。
type pruneEnv struct {
	panel   *Panel
	pool    *pool.Pool
	authDir string
}

func newPruneEnv(t *testing.T) *pruneEnv {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := pool.New(filepath.Join(dir, "state.json"))
	add := func(uid string, credits int64) string {
		fp := filepath.Join(authDir, "workbuddy-"+uid+".json")
		if err := os.WriteFile(fp, []byte(`{"uid":"`+uid+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		p.Add(&auth.Auth{UID: uid, FilePath: fp})
		p.SetCredits(uid, credits, 0)
		return fp
	}
	add("b1", 4300)
	add("b2", 1200)
	add("manual", 900)
	add("trial", 300)
	add("ok", 2000)
	p.Disable("b1", bannedReason)
	p.Disable("b2", "account banned by upstream (11140 request illegal), re-login required")
	p.Disable("manual", "manual disable (panel)")
	p.Disable("trial", "14017 trial not activated")

	return &pruneEnv{
		panel:   New(Config{Version: "test", APIKey: "test-key", Pool: p, AuthDir: authDir}),
		pool:    p,
		authDir: authDir,
	}
}

func (e *pruneEnv) post(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/accounts/prune_banned", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *pruneEnv) ids(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, st := range e.pool.List() {
		out = append(out, st.UID)
	}
	return out
}

func hasUID(ids []string, uid string) bool {
	for _, u := range ids {
		if u == uid {
			return true
		}
	}
	return false
}

// 空 body 必须落到「只读预演」：接口会删凭证，默认动作只能是不可逆操作的反面。
func TestPruneBannedDefaultsToDryRun(t *testing.T) {
	e := newPruneEnv(t)

	code, out := e.post(t, "")
	if code != 200 {
		t.Fatalf("预演应 200，得到 %d: %v", code, out)
	}
	if out["dry_run"] != true {
		t.Fatalf("空 body 必须按预演处理，得到 dry_run=%v", out["dry_run"])
	}
	if got := out["count"]; got != float64(2) {
		t.Errorf("命中数应为 2（b1/b2），得到 %v", got)
	}
	if got := out["frozen_credits"]; got != float64(5500) {
		t.Errorf("冻结积分应为 5500，得到 %v", got)
	}
	// 预演不得动池，也不得动文件。
	if n := len(e.pool.List()); n != 5 {
		t.Errorf("预演后账号数应仍为 5，得到 %d", n)
	}
	for _, uid := range []string{"b1", "b2"} {
		if _, err := os.Stat(filepath.Join(e.authDir, "workbuddy-"+uid+".json")); err != nil {
			t.Errorf("预演不该动凭证文件 %s: %v", uid, err)
		}
	}
	// 归档目录也不能被预演创建出来（预演是纯读）。
	if entries, err := os.ReadDir(filepath.Join(filepath.Dir(e.authDir), "auths-pruned")); err == nil && len(entries) > 0 {
		t.Errorf("预演不该创建归档目录内容: %v", entries)
	}
}

// 真删：只动上游封禁号，人工禁用 / 14017 / 健康号必须留在池子里，凭证归档不删除。
func TestPruneBannedRemovesOnlyBanned(t *testing.T) {
	e := newPruneEnv(t)

	code, out := e.post(t, `{"dry_run":false}`)
	if code != 200 {
		t.Fatalf("剔除应 200，得到 %d: %v", code, out)
	}
	if got := out["removed"]; got != float64(2) {
		t.Fatalf("应剔除 2 个，得到 %v（%v）", got, out)
	}
	if got := out["archived"]; got != float64(2) {
		t.Errorf("应归档 2 份凭证，得到 %v", got)
	}

	ids := e.ids(t)
	if hasUID(ids, "b1") || hasUID(ids, "b2") {
		t.Errorf("封禁号应已出池: %v", ids)
	}
	for _, keep := range []string{"manual", "trial", "ok"} {
		if !hasUID(ids, keep) {
			t.Errorf("%s 不该被剔除，现有账号: %v", keep, ids)
		}
	}

	archDir := filepath.Join(filepath.Dir(e.authDir), "auths-pruned")
	matches, _ := filepath.Glob(filepath.Join(archDir, "*", "workbuddy-b1.json"))
	if len(matches) != 1 {
		t.Errorf("b1 凭证应归档到 %s/*/workbuddy-b1.json，实际 %v", archDir, matches)
	}
	if _, err := os.Stat(filepath.Join(e.authDir, "workbuddy-b1.json")); !os.IsNotExist(err) {
		t.Errorf("b1 凭证应已从 auth_dir 移走（否则重启会被 LoadDir 载回），err=%v", err)
	}
	// 归档目录与 auth_dir 并列而非其子目录：LoadDir 只 glob auth_dir/workbuddy*.json，
	// 放子目录将来一旦改成递归加载就会把刚剔掉的号载回来。
	if rel, err := filepath.Rel(e.authDir, archDir); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("归档目录不应位于 auth_dir 内部，rel=%s", rel)
	}
}

// uids 白名单：只删确认框里看到的那批，期间新被封的号留到下一次。
func TestPruneBannedHonorsUIDList(t *testing.T) {
	e := newPruneEnv(t)

	code, out := e.post(t, `{"dry_run":false,"uids":["b1","manual","ghost"]}`)
	if code != 200 {
		t.Fatalf("剔除应 200，得到 %d: %v", code, out)
	}
	if got := out["removed"]; got != float64(1) {
		t.Fatalf("只应剔除 b1 一个，得到 %v（%v）", got, out)
	}
	ids := e.ids(t)
	if hasUID(ids, "b1") {
		t.Errorf("b1 应已出池: %v", ids)
	}
	if !hasUID(ids, "b2") {
		t.Errorf("b2 不在确认清单里，不该被删: %v", ids)
	}
	if !hasUID(ids, "manual") {
		t.Errorf("人工禁用号在清单里也不该被删: %v", ids)
	}
}

// overview 的 banned 是「可一键剔除」的口径，必须小于等于 disabled（含人工禁用等）。
func TestOverviewBannedCount(t *testing.T) {
	e := newPruneEnv(t)

	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("overview 应 200，得到 %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["banned"] != float64(2) {
		t.Errorf("banned 应为 2，得到 %v", out["banned"])
	}
	if out["disabled"] != float64(4) {
		t.Errorf("disabled 应为 4（含人工禁用/14017），得到 %v", out["disabled"])
	}
}
