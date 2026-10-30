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

// deadEnv 造一个「2 个 401 invalid_token 打标的号 + 1 个历史 session_dead 禁用号 +
// 1 个人工禁用 + 1 个 14017 + 1 个健康号」的最小池，每个号都有真实落盘的 auth 文件
// （剔除要归档文件，必须走文件系统）。
type deadEnv struct {
	panel   *Panel
	pool    *pool.Pool
	authDir string
}

func newDeadEnv(t *testing.T) *deadEnv {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := pool.New(filepath.Join(dir, "state.json"))
	add := func(uid string, credits int64) {
		fp := filepath.Join(authDir, "workbuddy-"+uid+".json")
		if err := os.WriteFile(fp, []byte(`{"uid":"`+uid+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		p.Add(&auth.Auth{UID: uid, FilePath: fp})
		p.SetCredits(uid, credits, 0)
	}
	add("d1", 500)
	add("d2", 700)
	add("legacy", 300) // 本次改动前就被 session_dead 禁掉的老号
	add("manual", 900)
	add("trial", 100)
	add("ok", 2000)

	// 新观测：401 invalid_token（MarkCredsDead 打标签 + 禁用）。
	p.MarkCredsDead("d1", "凭证已失效（401 invalid_token）")
	p.MarkCredsDead("d2", "凭证已失效（401 invalid_token）")
	// 历史形态：只有禁用原因，没有标签字段。
	p.Disable("legacy", "12153 session dead")
	p.Disable("manual", "manual disable (panel)")
	p.Disable("trial", "14017 trial not activated")

	return &deadEnv{
		panel:   New(Config{Version: "test", APIKey: "test-key", Pool: p, AuthDir: authDir}),
		pool:    p,
		authDir: authDir,
	}
}

func (e *deadEnv) post(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/accounts/prune_session_dead", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *deadEnv) ids(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, st := range e.pool.List() {
		out = append(out, st.UID)
	}
	return out
}

// 空 body 必须落到「只读预演」：这个接口会删凭证，默认动作只能是不可逆操作的反面。
func TestPruneSessionDeadDefaultsToDryRun(t *testing.T) {
	e := newDeadEnv(t)

	code, out := e.post(t, "")
	if code != 200 {
		t.Fatalf("预演应 200，得到 %d: %v", code, out)
	}
	if out["dry_run"] != true {
		t.Fatalf("空 body 必须按预演处理，得到 dry_run=%v", out["dry_run"])
	}
	// d1/d2（新标签）+ legacy（历史原因反推）= 3；人工禁用/14017/健康号不命中。
	if got := out["count"]; got != float64(3) {
		t.Errorf("命中数应为 3（d1/d2/legacy），得到 %v", got)
	}
	if got := out["frozen_credits"]; got != float64(1500) {
		t.Errorf("冻结积分应为 1500，得到 %v", got)
	}
	if n := len(e.pool.List()); n != 6 {
		t.Errorf("预演后账号数应仍为 6，得到 %d", n)
	}
	for _, uid := range []string{"d1", "d2", "legacy"} {
		if _, err := os.Stat(filepath.Join(e.authDir, "workbuddy-"+uid+".json")); err != nil {
			t.Errorf("预演不该动凭证文件 %s: %v", uid, err)
		}
	}
}

// 真删：只动凭证失效号（含历史反推的），人工禁用 / 14017 / 健康号必须留在池子里。
func TestPruneSessionDeadRemovesOnlyDead(t *testing.T) {
	e := newDeadEnv(t)

	code, out := e.post(t, `{"dry_run":false}`)
	if code != 200 {
		t.Fatalf("剔除应 200，得到 %d: %v", code, out)
	}
	if got := out["removed"]; got != float64(3) {
		t.Fatalf("应剔除 3 个，得到 %v（%v）", got, out)
	}
	if got := out["archived"]; got != float64(3) {
		t.Errorf("应归档 3 份凭证，得到 %v", got)
	}

	ids := e.ids(t)
	for _, gone := range []string{"d1", "d2", "legacy"} {
		if hasUID(ids, gone) {
			t.Errorf("凭证失效号应已出池: %s（现有 %v）", gone, ids)
		}
	}
	for _, keep := range []string{"manual", "trial", "ok"} {
		if !hasUID(ids, keep) {
			t.Errorf("%s 不该被剔除，现有账号: %v", keep, ids)
		}
	}

	// 凭证归档到 auth_dir 之外（否则重启 LoadDir 会把刚剔掉的号载回来）。
	archDir := filepath.Join(filepath.Dir(e.authDir), "auths-pruned")
	matches, _ := filepath.Glob(filepath.Join(archDir, "*", "workbuddy-d1.json"))
	if len(matches) != 1 {
		t.Errorf("d1 凭证应归档到 %s/*/workbuddy-d1.json，实际 %v", archDir, matches)
	}
	if _, err := os.Stat(filepath.Join(e.authDir, "workbuddy-d1.json")); !os.IsNotExist(err) {
		t.Errorf("d1 凭证应已从 auth_dir 移走，err=%v", err)
	}
}

// uids 白名单：只删确认框里看到的那批，且清单里混进非命中号也不能被误删。
func TestPruneSessionDeadHonorsUIDList(t *testing.T) {
	e := newDeadEnv(t)

	code, out := e.post(t, `{"dry_run":false,"uids":["d1","manual","ghost"]}`)
	if code != 200 {
		t.Fatalf("剔除应 200，得到 %d: %v", code, out)
	}
	if got := out["removed"]; got != float64(1) {
		t.Fatalf("只应剔除 d1 一个，得到 %v（%v）", got, out)
	}
	ids := e.ids(t)
	if hasUID(ids, "d1") {
		t.Errorf("d1 应已出池: %v", ids)
	}
	if !hasUID(ids, "d2") {
		t.Errorf("d2 不在确认清单里，不该被删: %v", ids)
	}
	if !hasUID(ids, "manual") {
		t.Errorf("人工禁用号在清单里也不该被删: %v", ids)
	}
}

// overview 的 session_dead 是「可一键剔除」的口径，必须与预演命中数一致。
func TestOverviewSessionDeadCount(t *testing.T) {
	e := newDeadEnv(t)

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
	if out["session_dead"] != float64(3) {
		t.Errorf("session_dead 应为 3，得到 %v", out["session_dead"])
	}
	// 与 banned 互不干扰：这批号没有一个是 11140 封禁。
	if out["banned"] != float64(0) {
		t.Errorf("banned 应为 0，得到 %v", out["banned"])
	}
}
