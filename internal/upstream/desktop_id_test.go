// desktop_id_test.go 钉住出站设备标识 / 请求 id 的**形态**。
//
// 为什么形态也算契约：官方桌面客户端的 machineId / sessionId 是带连字符的
// UUIDv4，X-Request-ID 是 32 位随机 hex。此前网关发的是 36 位无连字符 hex
// 与「派生前缀+微秒」串——取值再稳定也没用，形状本身就把它和真机分开，
// 是一条可直接匹配的网关特征（同批修正见 desktop.go 的 deriveUUID）。
package upstream

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// uuidV4 官方形态：8-4-4-4-12，第三段以 4 开头，第四段以 8/9/a/b 开头。
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDeriveUUIDShapeAndStability(t *testing.T) {
	a := &auth.Auth{UID: "uid-alpha"}
	for _, salt := range []string{"machine", "session", "webmachine"} {
		got := deriveUUID(a, salt)
		if !uuidV4.MatchString(got) {
			t.Errorf("deriveUUID(%q)=%q 不是 UUIDv4 形状", salt, got)
		}
		if again := deriveUUID(a, salt); again != got {
			t.Errorf("deriveUUID(%q) 不幂等：%q != %q", salt, got, again)
		}
	}
	// 同一账号的不同用途必须彼此不同（machineId 与 sessionId 共值会成为关联键）。
	if deriveUUID(a, "machine") == deriveUUID(a, "session") {
		t.Error("machineId 与 sessionId 撞值，二者必须独立派生")
	}
	// 跨账号必须不同（这正是「一账号一台设备」的隔离口径）。
	b := &auth.Auth{UID: "uid-beta"}
	if deriveUUID(a, "machine") == deriveUUID(b, "machine") {
		t.Error("不同账号派生出同一 machineId，隔离失效")
	}
}

func TestDesktopRequestIDIsRandomHex(t *testing.T) {
	var ids []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		ids = append(ids, r.Header.Get("X-Request-ID"))
		if ua := r.Header.Get("User-Agent"); ua == "" {
			t.Error("桌面上报缺 UA")
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	a := &auth.Auth{UID: "uid-alpha", AccessToken: "at", Nickname: "n"}
	for i := 0; i < 2; i++ {
		if err := c.ReportDesktopEvent(a, DesktopEvent{"eventCode": "chat_request_send"}); err != nil {
			t.Fatalf("ReportDesktopEvent: %v", err)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("got %d requests want 2", len(ids))
	}
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for i, id := range ids {
		if !hex32.MatchString(id) {
			t.Errorf("X-Request-ID[%d]=%q 不是 32 位 hex（真客户端每条随机生成）", i, id)
		}
	}
	if ids[0] == ids[1] {
		t.Error("两条请求用了同一个 X-Request-ID：稳定前缀是网关特征")
	}
}

// TestDesktopFingerprintUsesUUID 指纹里的 machineId / sessionId 也必须已经是
// UUID 形态——它们是桌面事件判据的一部分，不能只修 header 忘了 body。
func TestDesktopFingerprintUsesUUID(t *testing.T) {
	a := &auth.Auth{UID: "uid-alpha", Nickname: "n"}
	fp := desktopFingerprint(a)
	for _, k := range []string{"machineId", "sessionId"} {
		v, _ := fp[k].(string)
		if !uuidV4.MatchString(v) {
			t.Errorf("%s=%q 不是 UUIDv4 形状", k, v)
		}
	}
	if fp["machineId"] == fp["sessionId"] {
		t.Error("machineId 与 sessionId 不能共用同一个值")
	}
}
