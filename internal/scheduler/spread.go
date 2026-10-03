// spread.go 账号铺开（反指纹）：把同批多账号的请求从「同一秒 · 同一顺序 · 等间隔」
// 打散成「每号各有作息 + 间隔不规则 + 顺序不定」。
//
// 动机（2026-10-03 实测）：7 个号共享同一个随机窗口槽位，签到循环内**一个 sleep
// 都没有**（14 个请求挤在 1 秒内），旅行/活跃是固定 800ms / 1500ms，且
// Pool.List() 走 sort.Strings → 每天同一秒、同一顺序、同一毫秒间隔。
// 这比「同一分钟」严重得多：真人不会连续一年以 800ms 精度按 UID 字典序打卡。
//
// 建模：真人每天作息**稳定**，但不同的人作息**错开**。所以要打散的是「整组同步」，
// 不是「每号规律」——
//   - 每号一个由 UID 派生的**稳定**日偏移 → 每号像有自己作息的独立用户；
//   - 偏移之上叠**随机抖动** → 每号也不是时钟；
//   - 遍历顺序**打乱** → 消除 UID 字典序特征；
//   - 账号间/账号内间隔改**随机** → 消除等间隔。
//
// 为什么偏移必须跨日稳定、不能每轮重随机：相邻两天的间隔
// = (base2+off) − (base1+off) = base2−base1，**偏移在差分里相互抵消**，
// 猫猫旅行 ~24h 的行程节奏完全不受影响。若改成每天重随机，间隔方差会叠加到
// base 之上，可能短于行程时长 → 状态卡 traveling → 当日领奖落空。
package scheduler

import (
	"context"
	"hash/fnv"
	"math/rand"
	"time"
)

// spreadEnabled 铺开是否生效（零值 Config 即关闭，行为与引入前逐字一致）。
func (s *Scheduler) spreadEnabled() bool {
	return s.cfg.AccountSpread > 0
}

// accountOffset 返回该账号在该任务族里的稳定日偏移 ∈ [0, AccountSpread)。
//
// 任务族（kind）参与哈希：签到/旅行/活跃/保活四个族彼此也错开，否则它们会
// 共享同一批偏移时刻，仍能从时间轴上看出「同一时刻四个动作」的规整形状。
func (s *Scheduler) accountOffset(uid, kind string) time.Duration {
	if !s.spreadEnabled() {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(uid))
	_, _ = h.Write([]byte{0}) // 分隔符，避免 ("ab","c") 与 ("a","bc") 撞哈希
	_, _ = h.Write([]byte(kind))
	return time.Duration(h.Sum64() % uint64(s.cfg.AccountSpread))
}

// accountJitter 在稳定偏移之上叠一层随机抖动，让每号也不是精确的钟。
func (s *Scheduler) accountJitter() time.Duration {
	if s.cfg.AccountJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(s.cfg.AccountJitter)))
}

// waitAccountTurn 在处理某账号前等待「稳定日偏移 + 随机抖动」。
// ctx 取消立即返回 false（优雅停机不必等铺开睡满，剩余账号下轮再巡）。
func (s *Scheduler) waitAccountTurn(ctx context.Context, uid, kind string) bool {
	d := s.accountOffset(uid, kind) + s.accountJitter()
	if d <= 0 {
		return ctx.Err() == nil
	}
	return sleepCtx(ctx, d)
}

// interAccountDelay 账号间随机间隔 ∈ [MinAccountDelay, MaxAccountDelay]。
// 取代原来的固定 800ms：等间隔本身就是最强的机器特征。
func (s *Scheduler) interAccountDelay() time.Duration {
	return randDelay(s.cfg.MinAccountDelay, s.cfg.MaxAccountDelay)
}

// accountDelay 账号间等待时长：铺开开启走随机区间，否则沿用传入的固定值。
// 固定值走包级变量（travelAccountDelay 等）——那既是旧行为，也是测试置 0 的钩子。
func (s *Scheduler) accountDelay(legacy *time.Duration) time.Duration {
	if !s.spreadEnabled() {
		return *legacy
	}
	return s.interAccountDelay()
}

// reportGapDelay 同一账号内连续上报之间的随机间隔，取代固定 1500ms。
func (s *Scheduler) reportGapDelay(legacy *time.Duration) time.Duration {
	if !s.spreadEnabled() {
		return *legacy
	}
	return randDelay(s.cfg.MinReportGap, s.cfg.MaxReportGap)
}

// randDelay 返回 [min,max] 闭区间内的随机时长。非正/倒置时退化为 min。
func randDelay(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int63n(int64(max-min)))
}

// maybeShuffle 遍历前打乱顺序，消除 Pool.List() 的 sort.Strings 字典序特征。
// 不修改入参（Pool.List() 每次返回新切片，但保持不改的惯例避免误用）。
func maybeShuffle[T any](in []T, enabled bool) []T {
	if !enabled || len(in) < 2 {
		return in
	}
	out := make([]T, len(in))
	copy(out, in)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// batchReserve 估计「一批维护任务」的最长占用时长。
//
// 存在的理由：Run 主循环是**阻塞式**的（runBatch 等所有任务族收尾才回主循环
// nextWake）。随机窗口基准时刻若不预留这段，批尾会落进夜猫窗口（23:00 起），
// 把夜猫子时点挤掉——那等于用「反指纹」换来「丢一个任务」。
// 故基准时刻只在 [start, end-reserve] 里取，保证整批在窗口内收尾。
//
// 估算式（宁可偏大，窗口本来就宽裕）：
//
//	4 族 × (铺开 + 抖动) + 账号数 × (账号间上限 + 上报条数 × 上报间隔上限)
func (s *Scheduler) batchReserve() time.Duration {
	if !s.spreadEnabled() {
		return 0
	}
	n := len(s.cfg.Pool.List())
	perAcct := s.cfg.MaxAccountDelay
	if n > 0 && s.cfg.ActivityReportCount > 0 {
		perAcct += time.Duration(s.cfg.ActivityReportCount) * s.cfg.MaxReportGap
	}
	return 4*(s.cfg.AccountSpread+s.cfg.AccountJitter) + time.Duration(n)*perAcct
}
