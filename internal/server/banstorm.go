// banstorm.go 11140 全局封禁风暴闸门（fleet-level ban storm gate）。
//
// 背景（2026-11-01 生产日志取证）：
//
//	2026-10-29 10:57:51 → 10:59:17（86 秒内）7 个账号**全部**命中 11140
//	（"request illegal" / "账号存在异常行为，已被限制使用"）；
//	2026-10-29 22:01 → 22:09 再次出现同样的集体封禁波。
//
//	对比 2026-10-31 09:00–12:00：506 个请求全部落在**同一个**账号上，零封禁。
//
// 判读：11140 的语义是**账号级**风控（2026-11-01 A/B 实测：同一个封禁账号换 4 种
// system（无 system / 中性句 / DSH 身份句 / 完整 DSH 全文）全部 11140；同一分钟内的
// 健康账号 0 次 11140 —— 与请求内容无关）。但**集体**封禁说明触发的是**账号集合 /
// 出口 IP / 客户端指纹**层面的判定，而不是某个账号自身有问题。
//
// 为什么需要这道闸门：单号封禁冷却（pool.CooldownBanned）只保证「撞到 11140 的号
// 进冷却」，但**轮转仍会继续**——一个请求依次试 A/B/C，A 撞封→冷却、B 撞封→冷却、
// C 撞封→冷却……于是一次客户端请求就能把整池烧光（10-29 就是这么烧的：86 秒烧掉 7 个）。
// 闸门在「短窗内多个**不同**账号命中 11140」时激活，**立即停止轮转**，不再把剩余
// 账号往上游送——止损优先于用光配额。
//
// 与 wafip.go 的关系：同一套范式（进程内状态、短窗多号计数、激活期 fail-fast、
// 重启清零），但判定对象不同——wafIP 判「出口 IP 被拦」（WAF 403 空 body），
// banStorm 判「账号集合被上游风控」（11140 带业务信封）。
//
// 归属层：放 server（Handler 局部）而非 pool——闸门唯一消费者是 chatCompletions
// 轮转循环（是否继续轮转 / 是否直接 fail-fast），账号级记账仍在 pool（CooldownBanned）。
// 与 wafip.go 同理：跨 UID 语义不属于任何账号。
package server

import (
	"log"
	"strings"
	"sync"
	"time"
)

// banStormWindow 风暴判定滑动窗 + 激活时长。
//
// 取 120s：2026-10-29 的集体封禁波是 86 秒内 7 个号（10:57:51→10:59:17）——
// 120s 窗能把整波收进来。取更小（如 60s）会漏掉 22:01–22:09 那波（8 分钟跨度）；
// 取更大（如 5min）则闸门开启过久，白白停服。var 仅供测试注入短窗。
var banStormWindow = 120 * time.Second

// banStormThreshold 判定阈值：窗内命中 11140 的**不同**账号数达到该值即判为
// 全局风暴并激活。
//
// 取 3 的理由：11140 天然是账号级——单个账号被判异常完全可能（该号本身有问题）。
// 「多个账号在短窗内接连被判异常」才是集合级信号。阈值 2 偏敏感（两个号各自
// 有问题就会触发，白停服 2 分钟）；3 能在 10-29 那种 7 连封里尽早止损——
// 7 个号里保住 4 个。
const banStormThreshold = 3

// banStormGate 11140 全局封禁风暴状态机（Handler 内嵌，零值可用）。
type banStormGate struct {
	mu    sync.Mutex
	hits  map[string]time.Time // uid → 最近一次 11140 时刻（判定窗内，惰性剪枝）
	until time.Time            // 风暴闸门激活截止；零值 = 未激活
}

// noteBan 记一次某账号的 11140，返回记账后风暴闸门是否已激活
// （调用方据此立即终止轮转，不再把剩余账号送上游）。
//
// 语义（与 wafip.go 的 noteWaf 同风格）：
//   - 已激活（now < until）：返回 true，不续期、不记账——激活期内不重置，
//     窗口自然解除（保守：不主动探测）。
//   - 未激活：记 hits[uid]=now（同号重复命中覆盖不累计，判定口径是「不同号数」），
//     剪掉窗外的过期命中；不同 UID 数达 banStormThreshold → 激活到 now+banStormWindow
//     （打一条 WARN 供观测），清空判定窗（解除后需全新命中重新判定，不叠旧账）。
func (g *banStormGate) noteBan(uid string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.until) {
		return true // 激活期内新命中：不续期（自然解除语义）
	}
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	g.hits[uid] = now
	for u, t := range g.hits {
		if now.Sub(t) > banStormWindow {
			delete(g.hits, u)
		}
	}
	if len(g.hits) >= banStormThreshold {
		g.until = now.Add(banStormWindow)
		log.Printf("WARN: [server] 11140 ban storm: %d distinct accounts restricted within %s, rotate fail-fast until %s (fleet-level risk control suspected; remaining accounts spared)",
			len(g.hits), banStormWindow, g.until.Format(time.RFC3339))
		g.hits = map[string]time.Time{}
		return true
	}
	return false
}

// active 报告风暴闸门是否激活（轮转循环入口处据此直接 fail-fast，不打上游）。
func (g *banStormGate) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// isBannedFault 判定上游错误体是否属于 11140 账号级封禁。
//
// 判据用 msg 里的 "request illegal"（与上游 Classify 的 accountFaultMarkers 同口径，
// 大小写不敏感）——code 字段可能被上游换形态，用 msg 更稳。
//
// 调用方：applyErrorPolicy（决定封禁冷却 vs 软冷却）与 banStorm 闸门记账，
// 两处必须同口径，否则会出现「按软冷却处理却按封禁记账」的自相矛盾。
func isBannedFault(body string) bool {
	return strings.Contains(strings.ToLower(body), "request illegal")
}
