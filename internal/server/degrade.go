package server

import (
	"sync"
	"time"
)

// degradeGate 降级状态机：passthrough 模式下请求被上游内容策略拦截时，
// 把**那一个 system 指纹**切换到 Degraded 中性提示词直到次日 00:00 CST。
//
// 定位：误报处理（与 internal/upstream/sanitize.go 同哲学）——内容拦截多为
// system 来源的指纹误报，换最小中性提示词即可绕开；不是对抗框架。
//
// ⚠️ 2026-10-21 改为**按指纹**（事故：降级期间 tian_yan 的【标的】事实块被静默丢弃）：
// 旧实现是进程级单一 until，任何一个客户端被拦一次 → 当天**全体**客户端的 system 都被
// Rewrite 成 Degraded。现场：2026-10-21 00:00:09 另一客户端触发降级，随后 tian_yan「问AI」
// 的 1643 字事实块（含【标的】云中马 603130）在网关处被替换成
// "You are a helpful assistant.…"，模型只看到「通俗解释这公司做什么的？」→ 反问「哪家公司？」；
// 而界面「本次提示词」照旧完整显示 —— 用户看到的就是「明明带了标的，模型却不知道」。
//
// 现在语义：
//   - 只降级**撞过墙的那一个 system 指纹**（prompt.SystemFingerprint），别的客户端逐字不碰；
//   - 无 system 的请求（指纹空串）**不参与降级**：没有原文可换，换与不换都解决不了
//     用户内容本身触发审核的问题（handler 侧直接用 sysKey!="" 把关）；
//   - 每个指纹的降级期到次日 00:00 CST 重置，进程重启清零（可接受：重启极少触发，
//     且 custom 模式根本不进降级路径）。
type degradeGate struct {
	mu    sync.Mutex
	state map[string]*degradeEntry
}

type degradeEntry struct {
	until time.Time
	// logged 保证「该指纹这次降级期内只打一行 apply 日志」——不刷屏，但一旦
	// 客户端 system 真被换掉，日志里一定留得下一行可追（旧实现完全静默）。
	logged bool
}

// ActiveFor 该指纹当前是否处于降级期（now < until）。过期顺手清掉，避免 map 长肉。
func (g *degradeGate) ActiveFor(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.state[key]
	if !ok {
		return false
	}
	if !time.Now().Before(entry.until) {
		delete(g.state, key)
		return false
	}
	return true
}

// TriggerFor 触发**该指纹**的降级，直到次日 00:00 CST（Asia/Shanghai）。
// 该指纹已在降级期内则不续期（保持最早触发点的 00:00 重置语义）。
func (g *degradeGate) TriggerFor(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		g.state = make(map[string]*degradeEntry)
	}
	now := time.Now()
	if entry, ok := g.state[key]; ok && now.Before(entry.until) {
		return
	}
	g.state[key] = &degradeEntry{until: nextMidnightCST(now)}
}

// ShouldLogApply 该指纹这次降级期内是否还没打过「system 已被替换」的日志；
// 首次返回 true 并置位（同一降级期内后续请求不再打）。
func (g *degradeGate) ShouldLogApply(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.state[key]
	if !ok || entry.logged {
		return false
	}
	entry.logged = true
	return true
}

// Deadline 该指纹的降级截止时刻（未触发返回零值）。仅日志用。
func (g *degradeGate) Deadline(key string) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if entry, ok := g.state[key]; ok {
		return entry.until
	}
	return time.Time{}
}

// nextMidnightCST 返回 now 之后最近的 Asia/Shanghai 00:00 时刻（纯函数，方便单测）。
//
// 边界语义：
//   - 23:59 → 次日 00:00（几秒后）
//   - 00:00 → 次日 00:00（刚过 00:00，下个零点是次日）
//
// 用固定 +08:00 偏移计算，避免依赖系统时区配置（容器/宿主机时区不确定）。
func nextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	// 把 now 转到 CST 视角取当天 00:00，再加一天；若该 00:00 <= now 则再加一天。
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
