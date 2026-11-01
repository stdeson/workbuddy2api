// migrate_banned.go 旧版 11140 永久禁用状态的迁移。
//
// 背景：2026-11-01 修复前，网关把上游 11140「账号存在异常行为，已被限制使用」
// 当成**凭证失效**，对账号执行永久 Disable（reason 固定文案见 legacyBannedReason）。
// 实测后果：7 个号里 6 个在 10-29 被永久摘除，此后**一个都没自动恢复**，池容量
// 塌缩到只剩 1 个号扛全量流量——而上游封禁其实会自愈，只是时刻不可预测。
//
// 修复后 11140 走「长冷却 + 半开探测」（见 CooldownBanned），账号到期自动回到
// 候选池试探一次。但**已落盘的旧状态**（disabled=true + 旧 reason 文案）不会被
// 新逻辑接管：这个账号根本不会被选中，自然也不会触发新的 11140，也就永远
// 恢复不了——形成「修复上线了但账号还是死的」陷阱。
//
// 本迁移在服务启动时跑一次：把「因旧 11140 文案被永久禁用」的账号解除禁用，
// 让它重新进入候选池——第一次若仍被封，会由新路径转入封禁冷却（6h 起、翻倍、
// 48h 封顶），到期自动半开探测；恢复即自动归队。
//
// 精确匹配旧 reason 文案：只迁旧 11140 造成的禁用，不碰其它禁用原因
// （session dead 12153 / 余额不足 / 运维手动停用）。
package pool

import (
	"log"
	"strings"
)

// legacyBannedReason 旧版 11140 永久禁用写入 state.json 的 reason 原文。
// 必须与历史实现逐字一致，否则迁移匹配不上（匹配不上 = 账号永远留在死状态）。
const legacyBannedReason = "account banned by upstream (11140 request illegal), re-login required"

// MigrateLegacyBannedDisabled 解除「旧 11140 永久禁用」的账号禁用位。
//
// 返回被迁移的账号数。调用时机：服务启动、状态恢复之后。
//
// 为什么必须做：旧实现把「暂时封禁」当成「凭证失效」写进 state.json；
// 新代码只管**新的** 11140 事件，旧的死状态没人碰——账号不会被选中 → 不会
// 触发新的 11140 → 永远不恢复。迁移把它们放回池子，让新路径接管。
//
// 安全性：解除禁用后该账号重新参与选号。若上游仍在封它，第一次请求会吃 11140
// 并转入封禁冷却（不是回到永久禁用）——最坏情况是多一次试探请求，不是永久损失。
func (p *Pool) MigrateLegacyBannedDisabled() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	migrated := 0
	for uid, e := range p.byUID {
		if !e.disabled {
			continue
		}
		if !strings.Contains(e.reason, legacyBannedReason) {
			continue // 不是旧 11140 造成的禁用（如 session dead / 余额不足）→ 不动
		}
		e.disabled = false
		e.reason = ""
		migrated++
		log.Printf("[pool] migrated legacy 11140 permanent disable → recoverable: uid=%s (now eligible; a still-restricted account will enter banned cooldown on first hit)", uid)
	}
	if migrated > 0 {
		p.dirty.Store(true)
	}
	return migrated
}
