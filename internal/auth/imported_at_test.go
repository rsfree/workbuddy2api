package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 进池时间的来源判定是新号任务静默期的地基：显式（导入动作写入）才可信，
// mtime 推断值必须永远不参与门控 —— 否则一次搬机器就把整池老号静默三天。
func TestImportedAtSource(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"x"},
		"account":{"uid":"u1","nickname":"n1"},"imported_at":"2026-10-24T10:17:00+08:00"}`)
	a, err := Parse(nested)
	if err != nil {
		t.Fatalf("parse nested: %v", err)
	}
	if got := a.ImportedAtTime(); !got.Equal(time.Date(2026, 10, 24, 2, 17, 0, 0, time.UTC)) {
		t.Fatalf("imported_at = %v", got)
	}
	if !a.ImportedAtExplicit() {
		t.Fatal("文件里的 imported_at 必须标记为可信来源")
	}

	flat := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u2","imported_at":"2026-10-20T08:00:00Z"}`)
	b, err := Parse(flat)
	if err != nil {
		t.Fatalf("parse flat: %v", err)
	}
	if b.ImportedAtTime().IsZero() || !b.ImportedAtExplicit() {
		t.Fatalf("扁平形 imported_at 解析失败: %v explicit=%v", b.ImportedAtTime(), b.ImportedAtExplicit())
	}

	// 没有该键 → 零值、不可信（LoadDir 之后才会用 mtime 兜底，仍不可信）。
	plain := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1},"account":{"uid":"u3"}}`)
	c, err := Parse(plain)
	if err != nil {
		t.Fatalf("parse plain: %v", err)
	}
	if !c.ImportedAtTime().IsZero() || c.ImportedAtExplicit() {
		t.Fatalf("无 imported_at 时应为零值且不可信，got %v explicit=%v", c.ImportedAtTime(), c.ImportedAtExplicit())
	}

	// 坏时间戳不能挡住加载：按缺失处理。
	bad := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1},"account":{"uid":"u4"},"imported_at":"not-a-time"}`)
	d, err := Parse(bad)
	if err != nil {
		t.Fatalf("坏时间戳不应导致解析失败: %v", err)
	}
	if !d.ImportedAtTime().IsZero() || d.ImportedAtExplicit() {
		t.Fatalf("坏时间戳应按缺失处理，got %v explicit=%v", d.ImportedAtTime(), d.ImportedAtExplicit())
	}
}

// EnsureImportedAt 幂等：已有值的号重复导入不刷新；SetImportedAtFromFile 永不可信。
func TestImportedAtWriters(t *testing.T) {
	a := &Auth{UID: "u1"}
	t1 := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	a.EnsureImportedAt(t1)
	if !a.ImportedAtTime().Equal(t1) || !a.ImportedAtExplicit() {
		t.Fatal("首次 EnsureImportedAt 应写入且标记可信")
	}
	a.EnsureImportedAt(t1.Add(48 * time.Hour))
	if !a.ImportedAtTime().Equal(t1) {
		t.Fatalf("重复 EnsureImportedAt 不应覆盖，got %v", a.ImportedAtTime())
	}

	b := &Auth{UID: "u2"}
	b.SetImportedAtFromFile(t1)
	if !b.ImportedAtTime().Equal(t1) {
		t.Fatal("mtime 兜底应写入时间")
	}
	if b.ImportedAtExplicit() {
		t.Fatal("mtime 兜底绝不能标记为可信来源")
	}
}

// SaveAtomic 只把可信来源的 imported_at 落盘；mtime 推断值不落盘（否则一次搬运
// 就把推断值固化成「显式导入」，静默期判定彻底失真）。
func TestSaveAtomicImportedAt(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, explicit bool) *Auth {
		p := filepath.Join(dir, name)
		a := &Auth{AccessToken: "at", RefreshToken: "rt", UID: "u-" + name, FilePath: p}
		if explicit {
			a.EnsureImportedAt(time.Date(2026, 10, 24, 10, 17, 0, 0, time.UTC))
		} else {
			a.SetImportedAtFromFile(time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC))
		}
		if err := a.SaveAtomic(); err != nil {
			t.Fatalf("save: %v", err)
		}
		return a
	}

	explicit := mk("explicit.json", true)
	raw, err := os.ReadFile(explicit.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["imported_at"]; !ok {
		t.Fatal("可信来源必须落盘 imported_at")
	}

	inferred := mk("inferred.json", false)
	raw2, _ := os.ReadFile(inferred.FilePath)
	var doc2 map[string]any
	_ = json.Unmarshal(raw2, &doc2)
	if _, ok := doc2["imported_at"]; ok {
		t.Fatal("mtime 推断值不得落盘 imported_at")
	}
}
