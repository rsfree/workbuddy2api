package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStateSnapshotRoundTrip 锁住「重启不丢绑定」这条语义：
// 快照 → 新进程恢复后，会话仍指向原账号、亲和仍指向原账号。
//
// 这是 2026-10-25 那次优化的核心：上游前缀缓存是账号级的，绑定一丢就等于全部冷启动。
func TestStateSnapshotRoundTrip(t *testing.T) {
	usable := func(string, string) bool { return true }
	r1 := New(Config{TTL: time.Minute, PrefixAffinity: true, StickyUsable: usable})
	r1.Bind("s1", "u1")
	r1.NoteAffinity("p1\x00cn:m", "u1")

	raw := r1.SnapshotState()
	if len(raw) == 0 {
		t.Fatal("snapshot must not be empty")
	}

	r2 := New(Config{TTL: time.Minute, PrefixAffinity: true, StickyUsable: usable})
	binds, affs := r2.RestoreState(raw)
	if binds != 1 || affs != 1 {
		t.Fatalf("restored binds=%d affs=%d want 1/1", binds, affs)
	}
	if uid, ok := r2.Peek("s1"); !ok || uid != "u1" {
		t.Fatalf("restored binding=%q ok=%v want u1", uid, ok)
	}
	if uid, ok := r2.AffinityFor("p1\x00cn:m", "cn:m"); !ok || uid != "u1" {
		t.Fatalf("restored affinity=%q ok=%v want u1", uid, ok)
	}

	// 键里的 NUL 必须无损往返：亲和键是 prefixKey + "\x00" + 规范模型名。
	var doc stateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("snapshot must be valid JSON: %v", err)
	}
	if len(doc.Affinity) != 1 || doc.Affinity[0].Key != "p1\x00cn:m" {
		t.Fatalf("affinity key mangled: %+v", doc.Affinity)
	}
}

// TestRestoreDropsExpired 过期项不能靠重启续命：否则早已不活跃的会话会长期占着账号，
// 新会话反而分不到「空闲号」（双段分配的前提是绑定表反映真实活跃度）。
func TestRestoreDropsExpired(t *testing.T) {
	now := time.Now()
	doc := stateDoc{
		Version: stateVersion,
		Binds: []stateBind{
			{Key: "fresh", UID: "u1", LastActive: now.Add(-10 * time.Second)},
			{Key: "stale", UID: "u2", LastActive: now.Add(-2 * time.Hour)},
		},
		Affinity: []stateAff{
			{Key: "p-fresh", UID: "u1", LastSeen: now.Add(-time.Minute)},
			{Key: "p-stale", UID: "u2", LastSeen: now.Add(-2 * time.Hour)},
		},
	}
	raw, _ := json.Marshal(doc)

	r := New(Config{
		TTL: time.Minute, PrefixAffinity: true, AffinityTTL: 20 * time.Minute,
		StickyUsable: func(string, string) bool { return true },
	})
	binds, affs := r.RestoreState(raw)
	if binds != 1 || affs != 1 {
		t.Fatalf("restored binds=%d affs=%d want 1/1 (stale must be dropped)", binds, affs)
	}
	if _, ok := r.Peek("stale"); ok {
		t.Error("expired binding must not be restored")
	}
	if _, ok := r.AffinityFor("p-stale", "m"); ok {
		t.Error("expired affinity must not be restored")
	}
}

// TestRestoreIgnoresUnknownVersion 版本不认识宁可按冷启动：用语义已变的旧绑定把
// 会话钉在错的账号上，比不绑更贵（错号上的缓存一定是冷的）。
func TestRestoreIgnoresUnknownVersion(t *testing.T) {
	raw, _ := json.Marshal(stateDoc{
		Version: stateVersion + 1,
		Binds:   []stateBind{{Key: "k", UID: "u", LastActive: time.Now()}},
	})
	r := New(Config{TTL: time.Minute, StickyUsable: func(string, string) bool { return true }})
	if binds, affs := r.RestoreState(raw); binds != 0 || affs != 0 {
		t.Fatalf("unknown version must restore nothing, got %d/%d", binds, affs)
	}
	if _, ok := r.Peek("k"); ok {
		t.Error("unknown-version snapshot must not be applied")
	}
	// 坏 JSON 同理（不能 panic）：按冷启动处理。
	if binds, _ := r.RestoreState([]byte("{not json")); binds != 0 {
		t.Fatal("malformed snapshot must restore nothing")
	}
}

// TestRestoreKeepsExistingBinding 内存里已有的键不被快照覆盖：进程内刚建立的落点
// 比上一次落盘的快照新（快照最多滞后一个落盘周期）。
func TestRestoreKeepsExistingBinding(t *testing.T) {
	raw, _ := json.Marshal(stateDoc{
		Version: stateVersion,
		Binds:   []stateBind{{Key: "s1", UID: "old", LastActive: time.Now()}},
	})
	r := New(Config{TTL: time.Minute, StickyUsable: func(string, string) bool { return true }})
	r.Bind("s1", "new")
	if binds, _ := r.RestoreState(raw); binds != 0 {
		t.Fatalf("existing binding must win, restored %d", binds)
	}
	if uid, _ := r.Peek("s1"); uid != "new" {
		t.Fatalf("binding=%q want new", uid)
	}
}

// TestStateFilePersistsAcrossRestart 端到端：后台落盘 → 新进程读文件恢复。
// 这条是「发版不再全量冷启动」的直接证据。
func TestStateFilePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_state.json")
	usable := func(string, string) bool { return true }

	r1 := New(Config{TTL: time.Minute, PrefixAffinity: true, StickyUsable: usable})
	r1.StartStateFile(path, 10*time.Millisecond)
	r1.Bind("d-abc", "u1")
	r1.NoteAffinity("p-x\x00cn:m", "u1")

	// 轮询等待落盘（不 sleep 固定值：给 2s 上限，落盘周期 10ms 富余足够）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			r1.StopStateFile()
			t.Fatal("state file never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r1.StopStateFile()

	r2 := New(Config{TTL: time.Minute, PrefixAffinity: true, StickyUsable: usable})
	r2.LoadStateFile(path)
	if uid, ok := r2.Peek("d-abc"); !ok || uid != "u1" {
		t.Fatalf("after restart binding=%q ok=%v want u1", uid, ok)
	}
	if uid, ok := r2.AffinityFor("p-x\x00cn:m", "cn:m"); !ok || uid != "u1" {
		t.Fatalf("after restart affinity=%q ok=%v want u1", uid, ok)
	}

	// 幂等：重复 StopStateFile / StartStateFile 不 panic、不重复起 goroutine。
	r2.StopStateFile()
	r2.StartStateFile(path, 10*time.Millisecond)
	r2.StartStateFile(path, 10*time.Millisecond)
	r2.StopStateFile()
}

// TestLoadStateFileMissing 文件不存在 = 冷启动，不能报错也不能 panic。
func TestLoadStateFileMissing(t *testing.T) {
	r := New(Config{TTL: time.Minute, StickyUsable: func(string, string) bool { return true }})
	r.LoadStateFile(filepath.Join(t.TempDir(), "nope.json"))
	r.LoadStateFile("")
	if r.Count() != 0 {
		t.Fatal("missing file must leave router empty")
	}
}

// TestSnapshotSkipsExpired 过期项不写进文件（文件不该越写越大）。
func TestSnapshotSkipsExpired(t *testing.T) {
	r := New(Config{TTL: time.Minute, PrefixAffinity: true, StickyUsable: func(string, string) bool { return true }})
	r.Bind("fresh", "u1")
	r.mu.Lock()
	r.entries["stale"] = entry{uid: "u2", lastActive: time.Now().Add(-time.Hour)}
	r.mu.Unlock()

	var doc stateDoc
	if err := json.Unmarshal(r.SnapshotState(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Binds) != 1 || doc.Binds[0].Key != "fresh" {
		t.Fatalf("snapshot must contain only fresh bindings: %+v", doc.Binds)
	}
}
