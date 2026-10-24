package session

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// prefixKeyPrefix 前缀指纹的命名空间前缀，与派生会话键（"d-"）区分开：
// 两者会同时进日志/面板，前缀不同才能一眼看出是哪一类键。
const prefixKeyPrefix = "p-"

// PrefixKey 计算请求「共享前缀」的指纹：system 提示词 + 工具声明。
//
// 为什么需要它（2026-10-25，前缀亲和）：
// 上游前缀缓存是**账号级 + 模型级**的，命中的前提是「这段前缀曾在这个账号上跑过」。
// 会话粘性只覆盖「同一个会话的多轮」，但一个客户端通常同时跑多个会话——不同
// conversation_id，或者干脆没有 conversation_id 而落到派生键（system + 首条 user）。
// 这些会话共享同一段很长的 system 提示词与同一个工具集，那段**共同前缀**却会在
// 每个账号上各自冷启动一次。线上实测换号请求的前缀命中率 34.2% 而非 0，正是
// 「被换到的账号里恰好有同前缀的别的会话」的痕迹——说明同前缀本来就会散开。
//
// 指纹刻意只取**共享部分**，不含首条 user 消息：
//   - 含 user 文本 → 与 deriveKey 同粒度，退化成「按会话」，失去聚合同前缀的意义
//   - 不含 system/tools → 所有请求混作一类，等于把流量随机压到一个号上
//
// 口径取舍（宁可粗一点）：system 取全文；tools 只取「名字 + 描述」而不取完整
// schema——schema 要额外 marshal，而工具集差异几乎总能在名字/描述上体现。指纹
// 碰撞（两套不同工具集算出同一键）的后果只是「共享了账号」，不产生错误响应，
// 最坏是多一点负载倾斜，属于可接受的下行风险。
//
// 取不到任何前缀成分（无 system 且无 tools）时返回空串 → 该请求不参与前缀亲和，
// 走原有分配，安全降级。
func PrefixKey(body []byte) string {
	obj := parseBody(body)
	if obj == nil {
		return ""
	}
	key, _ := prefixPartsOf(obj)
	return key
}

// PrefixKeySized 同 PrefixKey，额外返回**前缀材料的字节数**（供观测「这段前缀
// 在一次请求里占多大比例」——亲和命中率单看没意义，prefix 占 prompt 很小的时候
// 命中率天然上不去，再怎么调路由也省不下钱）。
func PrefixKeySized(body []byte) (string, int) {
	obj := parseBody(body)
	if obj == nil {
		return "", 0
	}
	return prefixPartsOf(obj)
}

// prefixKeyOf 是 PrefixKey 的已解析版本（供 ExtractKeys 复用同一次解析）。
func prefixKeyOf(obj map[string]any) string {
	key, _ := prefixPartsOf(obj)
	return key
}

// prefixPartsOf 一次算出「指纹 + 材料字节数」，返回 ("", 0) 表示没有前缀成分。
//
// 字节数是**保守下界**：tools 只算名字与描述（schema 不入指纹，见上方口径取舍），
// 所以真实可缓存前缀 ≥ 这个值。用它算比例只会低估前缀占比，不会把「前缀其实很小」
// 误报成「前缀很大」——方向对安全。
func prefixPartsOf(obj map[string]any) (string, int) {
	var b strings.Builder
	// system：messages 里的 system/developer（多段全拼，顺序保留），再加顶层
	// system 字段（Anthropic 形态）。developer 与 system 在上游等价，同一个客户端
	// 新旧协议切换时不应因此换指纹。
	if msgs, ok := obj["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role := strOrEmpty(msg["role"])
			if role != "system" && role != "developer" {
				continue
			}
			b.WriteString(messageText(msg["content"]))
			b.WriteByte(0)
		}
	}
	if s := obj["system"]; s != nil {
		b.WriteString(messageText(s))
		b.WriteByte(0)
	}
	// tools：按名字排序后入「名字 + 描述」。客户端可能重排声明顺序（map 序列化、
	// 动态拼装），不排序会让同工具集算出不同指纹，亲和直接失效。
	if tools, ok := obj["tools"].([]any); ok && len(tools) > 0 {
		type toolSig struct{ name, desc string }
		list := make([]toolSig, 0, len(tools))
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			list = append(list, toolSig{strOrEmpty(tm["name"]), strOrEmpty(tm["description"])})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].name != list[j].name {
				return list[i].name < list[j].name
			}
			return list[i].desc < list[j].desc
		})
		for _, it := range list {
			b.WriteString(it.name)
			b.WriteByte(0)
			b.WriteString(it.desc)
			b.WriteByte(0)
		}
	}
	if b.Len() == 0 {
		return "", 0
	}
	sum := sha256.Sum256([]byte(b.String()))
	return prefixKeyPrefix + hex.EncodeToString(sum[:12]), b.Len()
}
