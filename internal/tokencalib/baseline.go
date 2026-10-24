package tokencalib

import "sync"

// Baseline 记住「某一批消息在上游的真实 prompt_tokens」，供 count_tokens 用
// 「真实基线 + 尾部增量」回答。
//
// 为什么需要它：count_tokens 是**请求之前**被调的（Claude Code 决定要不要压缩
// 上下文时先探一次），那一刻上游还没算过，当次的真实值根本不存在。唯一能拿到的
// 真实值是**历史响应**里的 usage.prompt_tokens。
//
// 而对话客户端（Claude Code / Codex）每轮发的是累积上下文：第 N 轮的 messages
// 是第 N-1 轮的 messages 后面追加若干条。于是可以把上一轮的真实值当前缀基线，
// 只对「这一轮新增的那几条消息」做字符估算：
//
//	当次输入 ≈ 上一轮真实 prompt_tokens + 新增消息的估算
//
// 相比「整段乘系数」（tokencalib.Calibrator），基线条目是上游真数，误差只剩新增
// 那一小段——而新增通常只有一条 user 消息。代价是需要按消息内容做前缀匹配：
// 上下文被压缩、换会话、客户端改了 system 时匹配不上，此时回落到系数换算。
//
// 除了「记真值」，它还负责**验证自己准不准**：count_tokens 给出预测后，客户端
// 通常紧接着用同一批 messages 发 chat，那次的 usage.prompt_tokens 就是这次预测的
// 标准答案，配对后按模型累计绝对误差（见 Snapshot 的 pred_* 字段）。线上判断
// 「这套估算到底能不能用、哪个模型偏得最多」靠的就是这组数，不需要额外埋点。
type Baseline struct {
	mu      sync.Mutex
	limit   int
	seq     uint64
	entries []baseEntry
	preds   []predEntry
	predsBy map[string]*predStat
	// onTail tail 段配对回调（Calibrator.ObserveTail）。启动时设一次，之后只读。
	onTail func(model string, rawEst, actualDelta int)
	hits   uint64
	misses uint64
	// oversize 因消息条数超 maxDigests 而放弃记录的轮数（长会话）。
	// 它是「该不该做滑动窗口」的唯一判据：持续增长说明长会话根本没基线可用。
	oversize uint64
}

type baseEntry struct {
	model   string
	digests []uint64
	tokens  int
	seq     uint64
}

// predEntry 一条「已给出预测、等真实值来打分」的记录。
//
// 不设 TTL 只设条数上限：客户端探完就去压缩上下文（messages 变短、前缀不再相等）
// 时这条永远等不到答案，靠先进先出顶掉，不会无限涨。
type predEntry struct {
	model     string
	digests   []uint64
	predicted int
	// base / tailRaw 供配对后单独学 tail 段的比例：真实增量 = actual - base，
	// 与 tailRaw 配成一对喂给 Calibrator.ObserveTail。完全命中时 tailRaw=0，不参与。
	base    int
	tailRaw int
}

// predStat 单个模型的预测误差累计。
type predStat struct {
	n      int
	absErr float64 // Σ |pred-actual| / actual
	worst  float64
}

const (
	// maxDigests 单条记录最多记多少条消息指纹。超过就不记录（不截断）：
	// 截断后「前 N 条匹配」仍会命中，但存的 tokens 对应的是完整 messages，
	// 两者错位会算出偏大的值。宁可放弃这一条，等下一轮短一点再记。
	maxDigests = 512
	// defaultBaselineLimit 默认保留多少个会话/批次。
	//
	// 1024 而不是更小：线上实测（68 账号的网关）每轮 chat 都会新增一条（messages 变长），
	// 旧轮次只能等 LRU 淘汰，一个活跃会话就会占好几条。32 条上线 3 分钟即打满，
	// 128 对多客户端并发仍偏紧——被淘汰的若是另一会话的最新记录，那次 count_tokens
	// 就白 miss。每条最多 512*8B ≈ 4KB，1024 条约 4MB，对常驻进程仍可接受。
	defaultBaselineLimit = 1024
	// maxPreds 同时挂多少条待验证预测。count_tokens 是客户端高频动作（每轮数次），
	// 只保留最近若干条足够配对——真正要验证的是「紧接着那次 chat」。
	maxPreds = 16
)

// NewBaseline 构造基线缓存。limit <= 0 时取默认值。
func NewBaseline(limit int) *Baseline {
	if limit <= 0 {
		limit = defaultBaselineLimit
	}
	return &Baseline{limit: limit, predsBy: make(map[string]*predStat)}
}

// Record 记录一批「消息指纹 → 上游真实 prompt_tokens」。
//
// 同 model 且指纹序列完全相同的记录会就地更新（客户端重试、并发重放不占新槽位）。
// tokens 非正数（上游没带 usage）时忽略：不能把「缺数据」记成 0。
func (b *Baseline) Record(model string, digests []uint64, tokens int) {
	if b == nil || model == "" || len(digests) == 0 {
		return
	}
	if tokens <= 0 {
		return
	}
	for _, d := range digests {
		// 指纹 0 来自序列化失败，无法区分消息，记进去会造成跨消息误匹配。
		if d == 0 {
			return
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 超长不记录（不截断）：截断后「前 N 条匹配」仍会命中，但存的 tokens 对应完整
	// messages，两者错位会算出偏大值。只计数，供 /status 判断要不要做滑动窗口。
	if len(digests) > maxDigests {
		b.oversize++
		return
	}

	b.seq++
	cp := append([]uint64(nil), digests...)

	// 先收账：这次的真值可能就是刚才某个 count_tokens 预测的标准答案。
	for i := 0; i < len(b.preds); i++ {
		p := b.preds[i]
		if p.model == model && equalDigests(p.digests, cp) {
			b.scorePredict(p, tokens)
			// 顺带把「新增段」的真实增量交给 tail 系数学习：base 是当时的基线真值，
			// 相减即本轮新增内容在上游的 token 数。回调只拿 Calibrator 自己的锁，
			// 不会反向调用 Baseline，所以在这里持锁调用不成环。
			if b.onTail != nil && p.base > 0 && p.tailRaw > 0 && tokens > p.base {
				b.onTail(p.model, p.tailRaw, tokens-p.base)
			}
			b.preds = append(b.preds[:i], b.preds[i+1:]...)
			break
		}
	}

	for i := range b.entries {
		if b.entries[i].model == model && equalDigests(b.entries[i].digests, cp) {
			b.entries[i].tokens = tokens
			b.entries[i].seq = b.seq
			return
		}
	}
	if len(b.entries) >= b.limit {
		oldest := 0
		for i := 1; i < len(b.entries); i++ {
			if b.entries[i].seq < b.entries[oldest].seq {
				oldest = i
			}
		}
		b.entries = append(b.entries[:oldest], b.entries[oldest+1:]...)
	}
	b.entries = append(b.entries, baseEntry{model: model, digests: cp, tokens: tokens, seq: b.seq})
}

// Predict 登记一条「这批消息我估成了 predicted」的待验证预测。
//
// 只在 count_tokens 真正给出数字后调用；随后同一批 messages 的 chat 响应回来时，
// Record 会自动配对打分。重复调用（客户端多次探同一批上下文）就地更新，不占新槽位。
// base 是预测时用到的基线真值，tailRaw 是新增段的字符粗估（未乘任何系数）——
// 两者用来在配对时单独给 tail 段学比例，不参与命中判定。
func (b *Baseline) Predict(model string, digests []uint64, predicted, base, tailRaw int) {
	if b == nil || model == "" || len(digests) == 0 || len(digests) > maxDigests {
		return
	}
	if predicted <= 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	cp := append([]uint64(nil), digests...)
	for i := range b.preds {
		if b.preds[i].model == model && equalDigests(b.preds[i].digests, cp) {
			b.preds[i].predicted = predicted
			b.preds[i].base = base
			b.preds[i].tailRaw = tailRaw
			return
		}
	}
	if len(b.preds) >= maxPreds {
		b.preds = append(b.preds[:0], b.preds[1:]...)
	}
	b.preds = append(b.preds, predEntry{model: model, digests: cp, predicted: predicted, base: base, tailRaw: tailRaw})
}

// SetTailObserver 注册「新增段配对」回调，把真实增量交给 tail 比例学习。
//
// 用回调而不是让 Baseline 直接持有 *Calibrator：两者解决的是不同问题（一个是
// 前缀缓存、一个是系数回归），绑在一起以后不好单独换掉或单独测试。
// 只在启动装配时调用一次；之后 Record 在锁内直接读这个字段。
func (b *Baseline) SetTailObserver(fn func(model string, rawEst, actualDelta int)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.onTail = fn
	b.mu.Unlock()
}

// scorePredict 把一次预测与它的真实值配对，累计误差。调用方已持锁。
func (b *Baseline) scorePredict(p predEntry, actual int) {
	if actual <= 0 || p.predicted <= 0 {
		return
	}
	err := float64(p.predicted-actual) / float64(actual)
	if err < 0 {
		err = -err
	}
	st := b.predsBy[p.model]
	if st == nil {
		st = &predStat{}
		b.predsBy[p.model] = st
	}
	st.n++
	st.absErr += err
	if err > st.worst {
		st.worst = err
	}
}

// Lookup 找与 digests 前缀匹配（记录是入参的前缀）的最长记录。
//
// 返回该前缀对应的真实 tokens 与匹配条数 matched（= len(记录)）。matched 等于
// len(digests) 表示完全命中——此时调用方应直接返回 tokens，不叠加任何估算。
//
// 多条同时匹配时取最长；长度相同取最新（客户端重试后 usage 可能更准）。
func (b *Baseline) Lookup(model string, digests []uint64) (tokens int, matched int, ok bool) {
	if b == nil || model == "" || len(digests) == 0 {
		return 0, 0, false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var bestSeq uint64
	for i := range b.entries {
		e := &b.entries[i]
		if e.model != model || len(e.digests) > len(digests) {
			continue
		}
		if !isPrefix(e.digests, digests) {
			continue
		}
		n := len(e.digests)
		if !ok || n > matched || (n == matched && e.seq > bestSeq) {
			tokens, matched, ok = e.tokens, n, true
			bestSeq = e.seq
		}
	}
	if ok {
		b.hits++
	} else {
		b.misses++
	}
	return tokens, matched, ok
}

// Snapshot 观测用：条目数、命中/未命中计数、以及预测误差。
//
// 三组数各有用途，线上排查时按顺序看：
//   - hits/misses —— 这套机制到底有没有生效。hits 恒为 0 说明客户端每轮都在改
//     system/messages（前缀永远匹配不上），此时应关掉基线、只用系数换算。
//   - pred_by_model.mae —— 命中了但估得准不准。mae 是 |预测-真实|/真实的均值，
//     0.05 表示平均差 5%；持续 > 0.15 说明「新增消息的字符估算」在该模型上偏得
//     厉害，该调 bridge.guessTokens 或改成对新增段也用系数换算。
//   - pred_pending —— 登记了预测却一直等不到真值的条数（客户端探完就压缩）。
func (b *Baseline) Snapshot() map[string]any {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	models := make(map[string]int, len(b.entries))
	for i := range b.entries {
		models[b.entries[i].model]++
	}
	verified := 0
	byModel := make(map[string]map[string]any, len(b.predsBy))
	for name, st := range b.predsBy {
		verified += st.n
		byModel[name] = map[string]any{
			"samples": st.n,
			"mae":     round4(st.absErr / float64(st.n)),
			"worst":   round4(st.worst),
		}
	}
	return map[string]any{
		"entries":       len(b.entries),
		"limit":         b.limit,
		"hits":          b.hits,
		"misses":        b.misses,
		"pred_verified": verified,
		"pred_pending":  len(b.preds),
		"pred_by_model": byModel,
		"oversize":      b.oversize,
	}
}

// round4 保留四位小数（0.0512 = 5.12%），避免 /status 里出现一长串浮点噪音。
func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

// isPrefix 判断 prefix 是否为 full 的前缀（prefix 可以更短）。
func isPrefix(prefix, full []uint64) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i := range prefix {
		if prefix[i] != full[i] {
			return false
		}
	}
	return true
}

// equalDigests 判断两个指纹序列是否完全相同。
func equalDigests(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
