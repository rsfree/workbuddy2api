// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	// group 出口分组（auth 文件 account.group / 顶层 group）。
	// 空 = 未分组，按账号维度分配出口。命名同 realm：持久化字段未导出，
	// 跨包读取走 Group()，避免与未来可能的方法名冲突。
	group    string
	FilePath string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// importedAt 首次进池时间（导入时间）。只写一次，token 刷新不改写 —— 语义是
	// 「这个号什么时候进池」，不是「最后一次写文件」。并发纪律同 group：加载期写、
	// 运行期只读，故不加锁。
	//
	// 两个来源必须区分（importedAtExplicit）：
	//   - 显式（true）：导入动作写入，会落盘进 auth 文件的顶层 imported_at 键，
	//     重启 / 搬机器后仍然可信 —— 新号任务静默期只认这个来源。
	//   - 推断（false）：老文件没有该键时用文件 mtime 兜底。mtime 在 rsync/scp
	//     -p 缺失、git checkout、重新落盘后都会集体漂移成"刚刚导入"（2026-10-24
	//     实例迁移实测：32 个老号的 imported_at 全塌到同一秒），一旦拿它判新号，
	//     整池老号会被误静默 3 天。故推断值只作展示，不参与门控。
	importedAt time.Time
	// importedAtExplicit importedAt 是否来自导入动作的可信来源（见上）。
	importedAtExplicit bool
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// ImportedAtTime 返回首次进池时间（零值 = 未知，前端显示 —）。
func (a *Auth) ImportedAtTime() time.Time {
	if a == nil {
		return time.Time{}
	}
	return a.importedAt
}

// SetImportedAtIfZero 仅在尚未记录时写入进池时间（幂等：启动加载 / 重复 upsert
// 不覆盖，保证「第一次进池」语义稳定）。不改变来源标记。
func (a *Auth) SetImportedAtIfZero(t time.Time) {
	if a == nil || t.IsZero() {
		return
	}
	if a.importedAt.IsZero() {
		a.importedAt = t
	}
}

// ImportedAtExplicit 报告 importedAt 是否来自「导入动作」的可信来源。
// false = 未知（零值）或由文件 mtime 推断（会随搬机器/重写漂移，不可信）。
func (a *Auth) ImportedAtExplicit() bool {
	if a == nil {
		return false
	}
	return a.importedAtExplicit
}

// EnsureImportedAt 导入动作专用：尚未记录进池时间时写入 t 并标记为可信来源。
// 幂等：已有值的号重复导入不刷新（"第一次进池"语义不变）。
// 与 SetImportedAtIfZero 的区别只在来源标记 —— 它就是调度器判断"新号"的依据。
func (a *Auth) EnsureImportedAt(t time.Time) {
	if a == nil || t.IsZero() {
		return
	}
	if a.importedAt.IsZero() {
		a.importedAt = t
		a.importedAtExplicit = true
	}
}

// SetImportedAtFromFile 加载期兜底：auth 文件没有显式 imported_at 键时用 mtime 推断。
// 只写值、**不打可信标记**（见 importedAt 注释：mtime 会集体漂移）。
func (a *Auth) SetImportedAtFromFile(t time.Time) {
	if a == nil || t.IsZero() {
		return
	}
	if a.importedAt.IsZero() {
		a.importedAt = t
	}
}

// Group 返回该账号的出口分组名（空 = 未分组）。
//
// 不加锁：group 只在 Parse/LoadDir 阶段写入，运行期只读（与 UID/Nickname 同
// 生命周期语义），没有 refresh 会改写它。
func (a *Auth) Group() string {
	if a == nil {
		return ""
	}
	return a.group
}

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争（go test -race 实证）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// BackfillRealmFor 显式写入 realm 标识（包外登录路径使用：panel login 已知用户选了
// global，直接落盘 realm=global，不依赖 domain 后缀推断）。realm 需为 cn/global，
// 非法值报错（防写脏）。返回是否发生变更。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// IsEnterprise 报告账号是否为企业版（auth 文件带非空 enterpriseId）。
//
// 企业版**没有个人成长体系**：签到 / 成长任务 / 连登管家 / 猫猫旅行 / 夜猫子
// 在上游一律被拒——实测（2026-10-07，同一时刻 A/B 对照）：
//   - POST /v2/billing/meter/daily-checkin                → 400 code 10001「企业账号不支持该操作」
//   - POST /billing/meter/claim-gift / claim-compensation → 400 code 10001 同上
//   - GET  /activity/growth/{streak,buddy/info,heatmap,lottery/summary}
//     与 GET /v2/activity/growth/tasks                    → 403「growth system is only available for personal users」
//
// 因此引用处（scheduler 五类任务 + panel 成长任务/签到入口）必须与 IsGlobal 门控
// 并列跳过，避免每日对注定失败的端点空发请求、面板按钮必然报错。
//
// 不受影响的能力：选号派发、保活（token 刷新）、额度查询（企业额度改走
// /v2/billing/meter/get-enterprise-user-usage，见 upstream.enterpriseResource）。
//
// 直读字段（EnterpriseID 自 Parse 后不再改写，与 upstream 读取方式一致），
// 无需加锁——Realm() 加锁是因为 realm/Domain 会被 RefreshToken 在锁内改写。
func (a *Auth) IsEnterprise() bool { return strings.TrimSpace(a.EnterpriseID) != "" }

// IsEnterpriseSession 报告当前凭证是否处于**真正绑定企业租户**的会话。
//
// 判定口径（2026-10-23 修正，重要）：
//   - 账号本身是企业版（IsEnterprise）→ true；
//   - JWT 带 token_source=enterprise_switch **且** 带非空 enterprise_id → true；
//   - 只有 token_source=enterprise_switch、enterprise_id 为空 → **false**。
//
// 为什么收紧：早前把「token_source=enterprise_switch」单独当作「行为事件被丢弃」
// 的充分条件，结果在一次 23 账号的线上排查里全员命中（22 个都是这个形态），
// 于是任务动作被 422 拦死。2026-10-23 对其中一个账号（enterprise_id 为空）做
// 对照实测，两条主判据都正常入账：
//   - web_element_click（资料库） 0/1 → 1/1
//   - chat_request_send（对话）   0/5 → 1/5
//
// 说明「没绑定租户的切换标记」不影响计分，只是 OAuth 走过企业空间。真正的企业
// 上下文必须带 enterprise_id——那才可能命中「读得见、记不上账」。
//
// 仍需本方法拦截的场景（企业租户绑定 / 真企业版）：/v2/report 返回 200 但事件
// 被静默丢弃（2026-10-12 实测），成长任务永远 0/x，不跳过则每轮白跑真实对话。
//
// 每次现解码 JWT 而非缓存：RefreshToken 会整体换发 token，企业绑定状态可能随
// 重新授权翻转。解码成本仅一次 base64 + 小 JSON，可忽略。
func (a *Auth) IsEnterpriseSession() bool {
	if a.IsEnterprise() {
		return true
	}
	src, eid := jwtClaims(a.AccessTokenValue())
	return src == "enterprise_switch" && strings.TrimSpace(eid) != ""
}

// EnterpriseSwitchOnly 凭证带 enterprise_switch 标记但**未绑定企业租户**
// （enterprise_id 为空）。这类号实测仍能正常计分（见 IsEnterpriseSession 注释里的
// 2026-10-23 实测），因此**不参与任何拦截**，仅供面板标注凭证来源。
func (a *Auth) EnterpriseSwitchOnly() bool {
	if a.IsEnterprise() {
		return false
	}
	src, eid := jwtClaims(a.AccessTokenValue())
	return src == "enterprise_switch" && strings.TrimSpace(eid) == ""
}

// jwtClaims 解出 JWT payload 里的企业相关声明。非 JWT / 解码失败一律返回空值
// （老 token 与非标准形态按无此声明处理，零回归）。先按无填充 base64url 解
// （JWT 标准），失败再试带填充形态（兼容非标准签发方）。
func jwtClaims(token string) (tokenSource, enterpriseID string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return "", ""
		}
	}
	var payload struct {
		TokenSource  string `json:"token_source"`
		EnterpriseID string `json:"enterprise_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", ""
	}
	return payload.TokenSource, payload.EnterpriseID
}

// jwtTokenSource 仅取 token_source（保留给日志/装饰用途）。
func jwtTokenSource(token string) string {
	src, _ := jwtClaims(token)
	return src
}

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
				Group        string `json:"group"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用；手写时无需嵌进 auth 对象）。
			DeviceToken string `json:"device_token"`
			// ImportedAt 顶层 imported_at（RFC3339）：由导入动作写入的可信进池时间。
			// 插件读取忽略未知键，写在顶层不影响桌面端注入。
			ImportedAt string `json:"imported_at"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			group:        n.Account.Group,
			DeviceToken:  n.DeviceToken,
		}
		a.applyImportedAt(n.ImportedAt)
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			Group        string `json:"group"`
			DeviceToken  string `json:"device_token"`
			ImportedAt   string `json:"imported_at"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			group:        f.Group,
			DeviceToken:  f.DeviceToken,
		}
		a.applyImportedAt(f.ImportedAt)
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// applyImportedAt 解析 auth 文件里的 imported_at 字符串（RFC3339/RFC3339Nano）。
// 解析失败视为缺失（退回 mtime 推断），不让一个坏时间戳挡住整个号加载。
func (a *Auth) applyImportedAt(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
	}
	if err != nil || t.IsZero() {
		return
	}
	a.importedAt = t
	a.importedAtExplicit = true
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	// group 非空才写回：避免在未分组的旧文件里引入空键（与 device_token 同纪律）。
	if a.group != "" {
		doc["account"].(map[string]any)["group"] = a.group
	}
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// 只写可信来源的进池时间：mtime 推断值不落盘（否则一次搬运就把推断值固化成
	// "显式导入"，静默期判定彻底失真）。
	if a.importedAtExplicit && !a.importedAt.IsZero() {
		doc["imported_at"] = a.importedAt.Format(time.RFC3339Nano)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		// Docker bind-mount 权限问题的典型现场：容器内 app 用户（uid 10001）
		// 对宿主机挂载目录无写权限。给出可操作指引而不是裸 syscall 错误。
		msg := fmt.Sprintf("写入 %s 失败: %v", tmp, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选：" +
				"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose 设 user: \"0:0\" 以 root 运行）"
		}
		return errors.New(msg)
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		// 导入时间：auth 文件里有显式 imported_at（导入动作写入）就信它；没有才用
		// 文件 mtime 兜底，且只当展示值 —— mtime 在搬机器/重写后会集体漂移，
		// 拿它判"新号"会把整池老号误静默（见 importedAt 注释）。
		if fi, err := os.Stat(f); err == nil {
			a.SetImportedAtFromFile(fi.ModTime())
		}
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
