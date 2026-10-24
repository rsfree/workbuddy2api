// Package tokencalib 用上游真实 prompt_tokens 校准本地的输入 token 粗估。
//
// 为什么需要：网关没有上游分词器，/v1/messages/count_tokens 只能按字符数
// 粗估（bridge.EstimateInputTokens）。误差有两个独立来源——
//
//  1. 字符/token 比随内容漂移：中文约 1.5、英文约 4、代码约 3.5；
//  2. 网关注入的 system 提示词是固定开销，粗估里根本没有（prompt_mode=custom
//     会整段替换客户端 system，两者还不对称）。
//
// 乘一个系数只能吃掉第 1 项；第 2 项与输入长度无关，乘系数会随输入变长被
// 稀释。所以这里拟合 actual ≈ slope*est + intercept 两个自由度：slope 描述
// 内容比，intercept 吸收固定开销。
//
// 观测源是每一次成功的对话请求（/v1/chat/completions，以及复用同一条链路的
// /v1/messages、/v1/responses）：请求进来时手里已有本地估算 est，响应结束时拿到
// 真实 usage.prompt_tokens，两者配对喂进来即可在线学习。采样点刻意放在 chat
// 链路的 usage 汇聚处而不是各协议前门——否则 Claude Code 之外的流量（占绝大多数）
// 全被浪费，系数要很久才收敛。
// 统计量带指数遗忘（等效样本量约 50 条），客户端内容分布变化后几十个请求内
// 就跟上；样本少时向「不变换」（slope=1, intercept=0）收缩，避免几条样本把
// 估算带偏——最坏情况就是不校准，不会比粗估更离谱。
package tokencalib

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// decay 每次观测对历史统计量的保留比例。0.98 ⇒ 等效样本量 ≈ 1/(1-0.98)
	// = 50 条：既能平滑噪声，又能在内容分布翻篇后几十个请求内跟上。
	decay = 0.98
	// priorN 先验证样本量：权重 w = 累计样本 / (累计样本 + priorN)，参数按 w 从
	// (1,0) 向拟合值过渡。取 4 是精度与稳健的折中：第 1 条样本只算 20% 权重，
	// 50 条到 0.93、200 条到 0.98。
	//
	// 注意权重必须用**累计**样本数而不是衰减后的 N：decay 把有效样本数锁死在
	// 1/(1-decay)=50 附近，若拿它算权重，先验永远退不干净——实测 49 条样本后
	// slope 仍被压在真值的 86%，长上下文永远偏小 14%，且再多的流量也修不回来。
	// 衰减只负责「让拟合跟得上内容分布变化」，不该影响置信度。
	priorN = 4.0
	// saveInterval 持久化最小间隔：每个请求都写盘没必要，进程退出前的最新
	// 几条观测丢一点也无关紧要（样本会继续累积）。
	saveInterval = 30 * time.Second
)

// 参数合理区间。clamp 的目的不是提高精度，而是挡住离谱观测（上游把缓存
// 命中算进 prompt_tokens 的比例突变、网关改了注入提示词长度等）把估算带飞。
const (
	minSlope     = 0.25
	maxSlope     = 6.0
	maxIntercept = 100000.0
	// ratioOnlyMinSlope / ratioOnlyMaxSlope 是「x 无方差」时允许的斜率带宽，
	// 对应字符/token 比的现实区间（见 params 里的注释）。
	ratioOnlyMinSlope = 0.5
	ratioOnlyMaxSlope = 2.0
)

// stats 单模型的在线回归统计量（正常方程求和，带指数遗忘）。
//
// 存求和而不是存最近 N 对样本，是为了 O(1) 内存：模型数固定但请求量可以
// 很大，留环形缓冲会让「最近 50 条」的语义依赖具体实现细节。
type stats struct {
	N   float64 `json:"n"`   // 衰减后的有效样本数
	Sx  float64 `json:"sx"`  // Σx
	Sy  float64 `json:"sy"`  // Σy
	Sxx float64 `json:"sxx"` // Σx²
	Sxy float64 `json:"sxy"` // Σxy
	// Total 累计观测次数（不衰减，仅用于日志/运维展示「学了多久」）。
	Total int64 `json:"total"`
}

// Params 单模型的校准参数（对外快照，用于 /status 与日志）。
type Params struct {
	Slope     float64 `json:"slope"`
	Intercept float64 `json:"intercept"`
	// Samples 累计观测次数；Weighted 是衰减后的有效样本数（回归实际权重）。
	Samples  int64   `json:"samples"`
	Weighted float64 `json:"weighted_samples"`
}

// Calibrator 按模型维护校准参数。零值不可用，用 New 构造。
type Calibrator struct {
	path string // 持久化路径；空串 = 只存内存

	mu       sync.Mutex
	models   map[string]*stats
	dirty    bool
	lastSave time.Time
}

// New 构造校准器并从 path 载入历史统计（path 为空或文件不存在时从零开始）。
// 载入失败不阻断启动：校准是「锦上添花」，坏了只是回退到粗估。
func New(path string) *Calibrator {
	c := &Calibrator{path: path, models: make(map[string]*stats)}
	if path == "" {
		return c
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[tokencalib] 读取 %s 失败（按无历史处理）: %v", path, err)
		}
		return c
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("[tokencalib] 解析 %s 失败（按无历史处理）: %v", path, err)
		return c
	}
	for model, s := range f.Models {
		if s != nil && s.N > 0 {
			c.models[model] = s
		}
	}
	return c
}

// file 持久化格式。version 留给以后改统计口径（换模型时旧文件直接忽略）。
type file struct {
	Version int               `json:"version"`
	Models  map[string]*stats `json:"models"`
}

const fileVersion = 1

// Adjust 把本地粗估 est 换算成校准后的 token 数。无历史时原样返回 est。
func (c *Calibrator) Adjust(model string, est int) int {
	if est <= 0 {
		return 0
	}
	if c == nil {
		return est
	}
	p := c.Params(model)
	v := p.Slope*float64(est) + p.Intercept
	if v < 1 {
		return 1
	}
	return int(math.Round(v))
}

// Observe 记录一对（本地估算, 上游实际 prompt_tokens）。任一侧非正数时忽略：
// 缺 usage 的响应（错误帧、上游没带 usage）不能当成 actual=0 学进去。
func (c *Calibrator) Observe(model string, est, actual int) {
	if c == nil || est <= 0 || actual <= 0 {
		return
	}
	c.mu.Lock()
	s := c.models[model]
	if s == nil {
		s = &stats{}
		c.models[model] = s
	}
	s.N = s.N*decay + 1
	s.Sx = s.Sx*decay + float64(est)
	s.Sy = s.Sy*decay + float64(actual)
	s.Sxx = s.Sxx*decay + float64(est)*float64(est)
	s.Sxy = s.Sxy*decay + float64(est)*float64(actual)
	s.Total++

	samples := s.Total
	p := s.params()
	c.dirty = true
	save := c.path != "" && time.Since(c.lastSave) >= saveInterval
	c.mu.Unlock()

	if save {
		c.Save()
	}
	if milestone(samples) {
		// 只在 1/5/20/50/每 100 次打一行：够运维判断「学到位了没」，
		// 又不会每个请求刷屏。
		log.Printf("[tokencalib] model=%s 样本=%d slope=%.2f intercept=%.0f（本次 est=%d actual=%d）",
			model, samples, p.Slope, p.Intercept, est, actual)
	}
}

// milestone 判断是否到了值得打日志的样本数。
func milestone(n int64) bool {
	switch n {
	case 1, 5, 20, 50:
		return true
	}
	return n%100 == 0
}

// Params 返回单模型当前参数（无历史时为 slope=1, intercept=0）。
func (c *Calibrator) Params(model string) Params {
	if c == nil {
		return Params{Slope: 1}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.models[model]; s != nil {
		return s.params()
	}
	return Params{Slope: 1}
}

// Snapshot 返回全模型参数快照（/status 展示用）。无历史时返回空 map 而非 nil。
func (c *Calibrator) Snapshot() map[string]Params {
	out := make(map[string]Params)
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for model, s := range c.models {
		out[model] = s.params()
	}
	return out
}

// params 由统计量解出回归参数（调用方需持锁或持有独占引用）。
func (s *stats) params() Params {
	p := Params{Slope: 1, Intercept: 0, Samples: s.Total, Weighted: s.N}
	if s.N <= 0 {
		return p
	}
	var a, b float64
	den := s.N*s.Sxx - s.Sx*s.Sx
	// den 是 x 的方差项（乘 N）。x 几乎恒定时它趋零，硬解会得到天文数字
	// 的 slope，此时只能定比例、截距归零。
	lo, hi := minSlope, maxSlope
	if den > 1e-6*s.N*s.Sxx && den > 0 {
		a = (s.N*s.Sxy - s.Sx*s.Sy) / den
		b = (s.Sy - a*s.Sx) / s.N
	} else if s.Sx > 0 {
		// x 没有方差（比如全是同一种短请求）：slope 与 intercept 不可辨识，
		// 比值里混着固定开销，单个样本能给出 50 这种荒谬的「斜率」。此时只允许
		// 比例在窄带内微调——字符/token 比的现实范围是 1.5~4（换算成相对粗估
		// 就是 0.75~2），超出这个带宽必是把 intercept 误当成了 slope。
		a = clamp(s.Sy/s.Sx, ratioOnlyMinSlope, ratioOnlyMaxSlope)
		lo, hi = ratioOnlyMinSlope, ratioOnlyMaxSlope
	}

	// 先验收缩：用累计样本数算权重（见 priorN 的注释）。
	w := float64(s.Total) / (float64(s.Total) + priorN)
	a = w*a + (1-w)*1
	b = w * b

	if math.IsNaN(a) || math.IsInf(a, 0) {
		a = 1
	}
	if math.IsNaN(b) || math.IsInf(b, 0) {
		b = 0
	}
	p.Slope = clamp(a, lo, hi)
	p.Intercept = clamp(b, 0, maxIntercept)
	return p
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Save 把当前统计量落盘（原子替换，0600）。并发安全；无脏数据时直接返回。
func (c *Calibrator) Save() {
	if c == nil || c.path == "" {
		return
	}
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	snapshot := make(map[string]*stats, len(c.models))
	for k, v := range c.models {
		cp := *v
		snapshot[k] = &cp
	}
	c.dirty = false
	c.lastSave = time.Now()
	c.mu.Unlock()

	raw, err := json.Marshal(file{Version: fileVersion, Models: snapshot})
	if err != nil {
		log.Printf("[tokencalib] 序列化失败: %v", err)
		return
	}
	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("[tokencalib] 创建目录 %s 失败: %v", dir, err)
			return
		}
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[tokencalib] 写入 %s 失败: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("[tokencalib] 替换 %s 失败: %v", c.path, err)
	}
}
