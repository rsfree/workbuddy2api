package session

import (
	"testing"
	"time"
)

// TestPrefixKeySharedAcrossConversations 锁住前缀指纹的核心语义：
// 同一段 system + tools、**不同用户消息**必须算出同一个指纹。
//
// 这正是会话派生键做不到的事：deriveKey 含首条 user 文本，所以「同一客户端并行
// 跑十个任务」会得到十个不同的键、散到十个账号上，那段几万 token 的共同 system
// 提示词因此被冷启动十次。前缀指纹只取共享部分，才能把它们撮合到同一个号。
func TestPrefixKeySharedAcrossConversations(t *testing.T) {
	base := `{"model":"m","messages":[
		{"role":"system","content":"you are a coding agent, rules..."},
		{"role":"user","content":"task A"}]}`

	other := `{"model":"m","messages":[
		{"role":"system","content":"you are a coding agent, rules..."},
		{"role":"user","content":"task totally different B"}]}`

	// 不同模型不影响前缀指纹：上游是账号级+模型级两份缓存，模型维度由调用方拼进
	// 亲和键（handler 里 prefixKey + "\x00" + 规范化模型名），指纹本身不含模型。
	samePrefixOtherModel := `{"model":"other","messages":[
		{"role":"system","content":"you are a coding agent, rules..."},
		{"role":"user","content":"task A"}]}`

	a, b, c := PrefixKey([]byte(base)), PrefixKey([]byte(other)), PrefixKey([]byte(samePrefixOtherModel))
	if a == "" {
		t.Fatal("prefix key should not be empty for a body with system prompt")
	}
	if a != b {
		t.Errorf("same shared prefix must yield same key: %q vs %q", a, b)
	}
	if a != c {
		t.Errorf("model name must not change the prefix key: %q vs %q", a, c)
	}
	if sessA, sessB := ExtractKey([]byte(base)), ExtractKey([]byte(other)); sessA == sessB {
		t.Fatalf("sanity: session keys must differ (%q) — otherwise this test proves nothing", sessA)
	}

	// 不同 system → 必须不同（否则等于把所有流量压到一个号上）。
	diffSystem := `{"messages":[
		{"role":"system","content":"you are a different agent"},
		{"role":"user","content":"task A"}]}`
	if d := PrefixKey([]byte(diffSystem)); d == a {
		t.Errorf("different system prompt must yield a different key")
	}
}

// TestPrefixKeyToolOrderInsensitive 锁住「工具声明顺序不影响指纹」。
// 客户端会用 map 序列化/动态拼装 tools，顺序不稳定；不排序会让同一工具集算出
// 不同指纹，亲和直接失效（而且是静默失效——面板只会显示亲和率上不去）。
func TestPrefixKeyToolOrderInsensitive(t *testing.T) {
	one := `{"messages":[{"role":"system","content":"s"}],"tools":[
		{"name":"read_file","description":"read a file"},
		{"name":"write_file","description":"write a file"}]}`
	two := `{"messages":[{"role":"system","content":"s"}],"tools":[
		{"name":"write_file","description":"write a file"},
		{"name":"read_file","description":"read a file"}]}`
	if a, b := PrefixKey([]byte(one)), PrefixKey([]byte(two)); a != b {
		t.Errorf("tool declaration order must not matter: %q vs %q", a, b)
	}

	// 只有 tools、没有 system 也要能算出指纹（纯工具型 agent）。
	bare := `{"tools":[{"name":"read_file","description":"read a file"}]}`
	if PrefixKey([]byte(bare)) == "" {
		t.Error("tools-only body should still produce a prefix key")
	}
}

// TestPrefixKeyEmpty 无任何前缀成分（无 system、无 tools）时返回空串：
// 这类请求不参与前缀亲和，走原有分配（安全降级，绝不把"空前缀"当成一类）。
func TestPrefixKeyEmpty(t *testing.T) {
	cases := []string{
		`{"messages":[{"role":"user","content":"hi"}]}`,
		`{"messages":[]}`,
		`{}`,
		``,
		`not json`,
	}
	for _, body := range cases {
		if got := PrefixKey([]byte(body)); got != "" {
			t.Errorf("body=%q should have empty prefix key, got %q", body, got)
		}
	}
}

// TestExtractKeysParsesOnce 保证合并解析的两条产出与各自独立调用一致
// （这是纯性能改动，语义必须逐字节对齐旧行为）。
func TestExtractKeysParsesOnce(t *testing.T) {
	body := []byte(`{"metadata":{"conversation_id":"c1","user_id":"u1"},
		"messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}]}`)
	sessKey, prefixKey := ExtractKeys(body)
	if sessKey != ExtractKey(body) {
		t.Errorf("session key mismatch: %q vs %q", sessKey, ExtractKey(body))
	}
	if prefixKey != PrefixKey(body) {
		t.Errorf("prefix key mismatch: %q vs %q", prefixKey, PrefixKey(body))
	}
	if sessKey != "c1" {
		t.Errorf("explicit conversation_id should win: %q", sessKey)
	}
}

// TestExtractKeysSized 锁住「前缀材料字节数」的口径：它是判断亲和含金量的分母。
//
// 字节数 = system 全文 + tools 名字/描述，各段用 \x00 分隔。刻意**不含** tools
// schema（schema 要额外 marshal，且几乎总能在名字/描述上区分）——所以它是真实
// 可缓存前缀的保守下界，方向安全：只会低估占比，不会把「前缀其实很小」报成很大。
func TestExtractKeysSized(t *testing.T) {
	// 10 字节 system + 1 个分隔符 = 11；无 tools。
	body := []byte(`{"metadata":{"conversation_id":"c1"},
		"messages":[{"role":"system","content":"abcdefghij"},{"role":"user","content":"U"}]}`)
	sessKey, prefixKey, n := ExtractKeysSized(body)
	if sessKey != "c1" {
		t.Errorf("session key=%q want c1", sessKey)
	}
	if prefixKey != PrefixKey(body) {
		t.Errorf("prefix key must match PrefixKey: %q vs %q", prefixKey, PrefixKey(body))
	}
	if n != 11 {
		t.Errorf("prefix bytes=%d want 11 (10 字节 system + 分隔符)", n)
	}

	// tools 的名字与描述都计入，且与声明顺序无关。
	var sized [2]int
	for i, b := range []string{
		`{"tools":[{"name":"a","description":"aaaa"},{"name":"b","description":"bb"}]}`,
		`{"tools":[{"name":"b","description":"bb"},{"name":"a","description":"aaaa"}]}`,
	} {
		_, key, sz := ExtractKeysSized([]byte(b))
		if key == "" || sz == 0 {
			t.Fatalf("tools-only body must produce key+size, got %q/%d", key, sz)
		}
		sized[i] = sz
	}
	if sized[0] != sized[1] {
		t.Errorf("prefix size must be order-independent: %d vs %d", sized[0], sized[1])
	}

	// 无前缀成分：键与体积都为零值（这类请求不参与亲和，也不该污染 prefix 占比）。
	if _, key, sz := ExtractKeysSized([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); key != "" || sz != 0 {
		t.Errorf("no-prefix body must be empty/0, got %q/%d", key, sz)
	}
	if _, _, sz := ExtractKeysSized([]byte(`not json`)); sz != 0 {
		t.Errorf("malformed body size=%d want 0", sz)
	}
}

// TestAffinityRegistry 覆盖亲和表本身的读写与三种失效路径。
func TestAffinityRegistry(t *testing.T) {
	usable := map[string]bool{"a": true, "b": true}
	r := New(Config{
		TTL:            time.Minute,
		Store:          newCountingStore(),
		PrefixAffinity: true,
		AffinityTTL:    50 * time.Millisecond,
		StickyUsable: func(uid, _ string) bool {
			return usable[uid]
		},
	})

	if _, ok := r.AffinityFor("p1", "m"); ok {
		t.Fatal("empty registry must miss")
	}
	r.NoteAffinity("p1", "a")
	if uid, ok := r.AffinityFor("p1", "m"); !ok || uid != "a" {
		t.Fatalf("affinity lookup=%q ok=%v want a", uid, ok)
	}
	if r.AffinityCount() != 1 {
		t.Fatalf("count=%d want 1", r.AffinityCount())
	}

	// 账号持久不可用（封禁/冷却/该模型限额）→ 命中失败并清掉记录，
	// 让下一次成功重建，而不是把前缀一直指向一个坏号。
	usable["a"] = false
	if _, ok := r.AffinityFor("p1", "m"); ok {
		t.Fatal("unhealthy affinity account must miss")
	}
	if r.AffinityCount() != 0 {
		t.Fatalf("unhealthy entry should be dropped, count=%d", r.AffinityCount())
	}

	// TTL 过期：亲和是共享状态，长期没在该号命中就该遗忘（比会话 TTL 短）。
	r.NoteAffinity("p2", "b")
	time.Sleep(70 * time.Millisecond)
	if _, ok := r.AffinityFor("p2", "m"); ok {
		t.Fatal("expired affinity must miss")
	}

	// 空 key / 空 uid 不写；关闭开关时整条机制不生效。
	r.NoteAffinity("", "a")
	r.NoteAffinity("p3", "")
	if r.AffinityCount() != 0 {
		t.Fatalf("empty key/uid must not be recorded, count=%d", r.AffinityCount())
	}
	off := New(Config{TTL: time.Minute, Store: newCountingStore(), PrefixAffinity: false})
	off.NoteAffinity("p1", "a")
	if _, ok := off.AffinityFor("p1", "m"); ok {
		t.Fatal("affinity must be inert when disabled")
	}
}

// TestAffinityCapacityEviction 保证注册表有界：键基数是「不同 system+tools 组合」
// 的数量，多客户端多模型场景可到千级，不设上限就是慢性内存泄漏。
func TestAffinityCapacityEviction(t *testing.T) {
	r := New(Config{
		TTL:            time.Minute,
		Store:          newCountingStore(),
		PrefixAffinity: true,
		AffinityMax:    8,
		StickyUsable:   func(string, string) bool { return true },
	})
	for i := 0; i < 8; i++ {
		r.NoteAffinity(string(rune('a'+i)), "u")
		time.Sleep(time.Millisecond) // 让 lastSeen 可比，淘汰才是确定的最旧优先
	}
	if r.AffinityCount() != 8 {
		t.Fatalf("count=%d want 8", r.AffinityCount())
	}
	r.NoteAffinity("overflow", "u")
	if n := r.AffinityCount(); n > 8 {
		t.Fatalf("registry exceeded cap: %d > 8", n)
	}
	// 最新写入的必须还在（淘汰的是最旧的一批，不能把刚建立的删掉）。
	if _, ok := r.AffinityFor("overflow", "m"); !ok {
		t.Error("newest entry must survive eviction")
	}
}
