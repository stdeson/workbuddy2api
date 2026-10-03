package scheduler

import (
	"context"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// realUIDs 是线上 7 个号的真实 uid——实测就是这 7 个号，取模方案在它们身上撞过。
var realUIDs = []string{
	"0c33a277-fc3d-4df4-a357-63126eeffef2",
	"20adac3a-fc1a-46ef-a332-bda3a34d99cf",
	"3bc5407f-e2f9-4613-8552-d01f166bd9a9",
	"721bdff9-9af1-4a83-ab1f-086d22271680",
	"a5a9a67d-e8bc-4bfa-a783-d076ced93669",
	"b9032a06-6053-48db-b4d4-ecbd9d767231",
	"ccfb20aa-1662-0862-2462-0862086224600",
}

var testKinds = []string{"checkin", "travel", "activity", "keepalive"}

// spreadCfg 开铺开的测试配置（分钟级铺开 + 秒级抖动，避免测试真的睡几分钟）。
func spreadCfg() Config {
	return Config{
		AccountSpread:   4 * time.Minute,
		AccountJitter:   90 * time.Second,
		MinAccountDelay: 3 * time.Second,
		MaxAccountDelay: 15 * time.Second,
		MinReportGap:    2 * time.Second,
		MaxReportGap:    6 * time.Second,
		ShuffleAccounts: true,
	}
}

// realPool 建线上 7 个真实 uid 的池。
func realPool(t *testing.T) *pool.Pool {
	t.Helper()
	p := pool.New("")
	for _, uid := range realUIDs {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	return p
}

// spreadSched 开铺开 + 线上 7 个真实账号的调度器。
func spreadSched(t *testing.T) *Scheduler {
	t.Helper()
	cfg := spreadCfg()
	cfg.Pool = realPool(t)
	return New(cfg)
}

// TestOffsetsNoCollision 7 个真实账号在同一任务族里必须落在**互不相同**的时刻。
//
// 这条是实测打出来的：初版用 hash 取模，线上 checkin 族真撞了一对同为 +3分28秒，
// 而「同一时刻」正是本次改造要消灭的东西。改等分槽位后必须零碰撞。
func TestOffsetsNoCollision(t *testing.T) {
	s := spreadSched(t)
	for _, kind := range testKinds {
		off := s.offsetsFor(kind)
		if len(off) != len(realUIDs) {
			t.Fatalf("%s: 偏移表 %d 项，池里 %d 个号", kind, len(off), len(realUIDs))
		}
		seen := map[time.Duration]string{}
		for uid, d := range off {
			if prev, dup := seen[d]; dup {
				t.Errorf("%s: %s 与 %s 撞在同一时刻 %v", kind, uid[:8], prev[:8], d)
			}
			seen[d] = uid
		}
	}
}

// TestOffsetsEvenlySpread 7 个号应等分铺满窗口：首号在 0、末号在第 6 格、不贴上界。
func TestOffsetsEvenlySpread(t *testing.T) {
	s := spreadSched(t)
	spread := s.cfg.AccountSpread
	off := s.offsetsFor("travel")
	var min, max time.Duration = spread, -1
	for _, d := range off {
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	if min != 0 {
		t.Errorf("最小偏移应为 0（铺满窗口起点），实际 %v", min)
	}
	step := spread / time.Duration(len(realUIDs))
	if want := step * time.Duration(len(realUIDs)-1); max != want {
		t.Errorf("最大偏移应等分到第 %d 格 %v，实际 %v", len(realUIDs)-1, want, max)
	}
	if max >= spread {
		t.Errorf("最大偏移 %v 触到上界 %v，会与下一批首尾相接", max, spread)
	}
}

// TestOffsetsStableAcrossDays 同一 (uid, 任务族) 的偏移必须**跨调用稳定**。
//
// 这是整个设计的地基：相邻两天的间隔 = (base₂+off)−(base₁+off) = base₂−base₁，
// 偏移在差分里相互抵消，猫猫旅行 ~24h 的行程节奏才不受影响。若改成每次重随机，
// 间隔方差会叠加到 base 之上，可能短于行程时长 → 状态卡 traveling → 当日领奖落空。
func TestOffsetsStableAcrossDays(t *testing.T) {
	s := spreadSched(t)
	for _, kind := range []string{"checkin", "travel"} {
		first := s.offsetsFor(kind)
		for i := 0; i < 200; i++ {
			got := s.offsetsFor(kind)
			for uid, d := range first {
				if got[uid] != d {
					t.Fatalf("%s/%s 偏移不稳定：第 %d 次 %v != 首次 %v",
						kind, uid[:8], i, got[uid], d)
				}
			}
		}
	}
}

// TestOffsetsVaryByTaskFamily 任务族参与哈希：签到/旅行/活跃不能共享同一批
// 偏移时刻，否则仍能从时间轴看出「同一时刻四个动作」的规整形状。
func TestOffsetsVaryByTaskFamily(t *testing.T) {
	s := spreadSched(t)
	ck, tv := s.offsetsFor("checkin"), s.offsetsFor("travel")
	same := 0
	for _, uid := range realUIDs {
		if ck[uid] == tv[uid] {
			same++
		}
	}
	if same == len(realUIDs) {
		t.Fatalf("签到/旅行偏移完全相同，任务族未参与哈希")
	}
}

// TestOffsetsInRange 所有偏移必须落在 [0, AccountSpread)。
func TestOffsetsInRange(t *testing.T) {
	s := spreadSched(t)
	for _, kind := range testKinds {
		for uid, d := range s.offsetsFor(kind) {
			if d < 0 || d >= s.cfg.AccountSpread {
				t.Errorf("%s/%s 偏移 %v 越界 [0,%v)", kind, uid[:8], d, s.cfg.AccountSpread)
			}
		}
	}
}

// TestOffsetsSingleAccount 单账号池：偏移 0（无处可铺），不得 panic。
func TestOffsetsSingleAccount(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "solo", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	cfg := spreadCfg()
	cfg.Pool = p
	s := New(cfg)
	if got := s.offsetsFor("travel")["solo"]; got != 0 {
		t.Errorf("单账号偏移应为 0，实际 %v", got)
	}
}

// TestOffsetsEmptyPool 空池不得 panic。
func TestOffsetsEmptyPool(t *testing.T) {
	cfg := spreadCfg()
	cfg.Pool = pool.New("")
	s := New(cfg)
	if got := s.offsetsFor("travel"); len(got) != 0 {
		t.Errorf("空池偏移表应为空，实际 %v", got)
	}
}

// TestSpreadDisabledIsZero 铺开关闭时偏移/抖动/间隔全为零值，行为与引入前逐字一致。
func TestSpreadDisabledIsZero(t *testing.T) {
	s := &Scheduler{cfg: Config{Pool: realPool(t)}} // AccountSpread=0 → 关闭
	if got := s.offsetsFor("checkin"); got != nil {
		t.Errorf("关闭时 offsetsFor 应返回 nil，实际 %v", got)
	}
	if got := s.accountJitter(); got != 0 {
		t.Errorf("关闭时抖动应为 0，实际 %v", got)
	}
	legacy := 800 * time.Millisecond
	if got := s.accountDelay(&legacy); got != legacy {
		t.Errorf("关闭时应沿用固定值 %v，实际 %v", legacy, got)
	}
	legacyGap := 1500 * time.Millisecond
	if got := s.reportGapDelay(&legacyGap); got != legacyGap {
		t.Errorf("关闭时应沿用固定间隔 %v，实际 %v", legacyGap, got)
	}
	if got := s.batchReserve(); got != 0 {
		t.Errorf("关闭时不应预留批次时长，实际 %v", got)
	}
}

// TestAccountDelayInRange 铺开开启后账号间间隔落在 [Min,Max] 且**不全相等**。
func TestAccountDelayInRange(t *testing.T) {
	s := spreadSched(t)
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		got := s.accountDelay(&travelAccountDelay)
		if got < s.cfg.MinAccountDelay || got > s.cfg.MaxAccountDelay {
			t.Fatalf("账号间间隔 %v 越界 [%v,%v]", got, s.cfg.MinAccountDelay, s.cfg.MaxAccountDelay)
		}
		seen[got] = true
	}
	if len(seen) < 10 {
		t.Errorf("200 次只取到 %d 个不同间隔，等间隔特征没消掉", len(seen))
	}
}

// TestReportGapInRange 账号内连续上报间隔同样随机且不越界。
func TestReportGapInRange(t *testing.T) {
	s := spreadSched(t)
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		got := s.reportGapDelay(&activityReportGap)
		if got < s.cfg.MinReportGap || got > s.cfg.MaxReportGap {
			t.Fatalf("上报间隔 %v 越界 [%v,%v]", got, s.cfg.MinReportGap, s.cfg.MaxReportGap)
		}
		seen[got] = true
	}
	if len(seen) < 3 {
		t.Errorf("200 次只取到 %d 个不同上报间隔", len(seen))
	}
}

// TestWaitAccountTurnCtxCancel ctx 取消必须立即返回 false，
// 否则优雅停机会被铺开的几分钟睡眠拖住。
func TestWaitAccountTurnCtxCancel(t *testing.T) {
	s := spreadSched(t)
	off := s.offsetsFor("checkin")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.waitAccountTurn(ctx, off, realUIDs[6]) {
		t.Error("ctx 已取消时 waitAccountTurn 应返回 false")
	}
}

// TestBatchReserveShrinksRandomWindow 随机窗口基准必须预留出一整批的时长，
// 否则批尾会落进夜猫窗口（23:00 起）把夜猫子时点挤掉——
// 等于用「反指纹」换来「丢一个任务」。
//
// 预留必须**向上取整到整分**：基准只精确到分，截断会漏出不足一分钟的尾巴
// （这个 bug 就是本用例第一次跑时抓出来的：24m27s 被截成 24 分钟，
// 基准落在 23:01 时 23:01+24m27s = 23:25:27 越界）。
func TestBatchReserveShrinksRandomWindow(t *testing.T) {
	cfg := spreadCfg()
	cfg.Pool = realPool(t)
	cfg.ActivityReportCount = 5
	cfg.RandomWindowStartMin = 8*60 + 33 // 08:33
	cfg.RandomWindowEndMin = 23*60 + 25  // 23:25
	s := New(cfg)

	if s.batchReserve() <= 0 {
		t.Fatalf("开启铺开后 batchReserve 应 >0，实际 %v", s.batchReserve())
	}
	limit := time.Date(2026, 10, 4, 23, 25, 0, 0, cstZone)
	start := time.Date(2026, 10, 4, 8, 33, 0, 0, cstZone)
	for i := 0; i < 2000; i++ {
		at := s.randomInWindow("2026-10-04")
		if at.Before(start) || at.Add(s.batchReserve()).After(limit) {
			t.Fatalf("基准 %s + 预留 %v 越出随机窗口 [08:33, 23:25]",
				at.Format("15:04"), s.batchReserve())
		}
	}
}

// TestBatchReserveDisabledIsZero 关闭铺开时不预留，随机窗口行为与引入前逐字一致。
func TestBatchReserveDisabledIsZero(t *testing.T) {
	s := &Scheduler{cfg: Config{Pool: realPool(t)}}
	if got := s.batchReserve(); got != 0 {
		t.Errorf("关闭铺开时 batchReserve 应为 0，实际 %v", got)
	}
}

// TestMaybeShufflePreservesInput 打乱不得修改入参，也不丢元素、不重复。
func TestMaybeShufflePreservesInput(t *testing.T) {
	in := []int{1, 2, 3, 4, 5, 6, 7}
	orig := append([]int(nil), in...)
	out := maybeShuffle(in, true)
	if len(out) != len(in) {
		t.Fatalf("长度变了：%d != %d", len(out), len(in))
	}
	sumIn, sumOut := 0, 0
	for _, v := range in {
		sumIn += v
	}
	for _, v := range out {
		sumOut += v
	}
	if sumIn != sumOut {
		t.Errorf("元素和变了：%d != %d", sumIn, sumOut)
	}
	for i := range in {
		if in[i] != orig[i] {
			t.Fatalf("入参被就地修改了：%v != %v", in, orig)
		}
	}
}

// TestMaybeShuffleDisabled 不打乱时原样返回。
func TestMaybeShuffleDisabled(t *testing.T) {
	in := []string{"a", "b", "c"}
	out := maybeShuffle(in, false)
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("关闭打乱却改了顺序：%v", out)
		}
	}
}

// TestRandDelayBounds randDelay 必须落在 [min,max]，倒置/相等时退化为 min。
func TestRandDelayBounds(t *testing.T) {
	min, max := 3*time.Second, 15*time.Second
	for i := 0; i < 300; i++ {
		got := randDelay(min, max)
		if got < min || got > max {
			t.Fatalf("randDelay %v 越界 [%v,%v]", got, min, max)
		}
	}
	if got := randDelay(max, min); got != max {
		t.Errorf("倒置时应退化为 min=%v，实际 %v", max, got)
	}
	if got := randDelay(min, min); got != min {
		t.Errorf("相等时应返回 min=%v，实际 %v", min, got)
	}
}

// TestStableHashDeterministic 哈希必须只由 (uid,kind) 决定，且分族、分号。
func TestStableHashDeterministic(t *testing.T) {
	a := stableHash("uid-1", "checkin")
	if a != stableHash("uid-1", "checkin") {
		t.Error("同一 (uid,kind) 哈希不稳定")
	}
	if a == stableHash("uid-1", "travel") {
		t.Error("不同任务族哈希相同，任务族未参与")
	}
	if stableHash("ab", "c") == stableHash("a", "bc") {
		t.Error("分隔符失效：(\"ab\",\"c\") 与 (\"a\",\"bc\") 撞哈希")
	}
}
