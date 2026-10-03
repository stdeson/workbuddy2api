package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// spreadCfg 开铺开的测试配置（秒级小值，避免测试真的睡几分钟）。
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

// TestAccountOffsetStablePerUID 同一 (uid,任务族) 的偏移必须**跨调用稳定**。
//
// 这是整个设计的地基：相邻两天的间隔 = (base2+off)−(base1+off) = base2−base1，
// 偏移在差分里抵消，猫猫旅行 ~24h 的行程节奏才不受影响。若这里改成每次重随机，
// 间隔方差会叠加到 base 之上，可能短于行程时长 → 状态卡 traveling → 当日领奖落空。
func TestAccountOffsetStablePerUID(t *testing.T) {
	s := &Scheduler{cfg: spreadCfg()}
	uid := "0c33a277-fc3d-4df4-a357-63126eeffef2"
	first := s.accountOffset(uid, "checkin")
	for i := 0; i < 200; i++ {
		if got := s.accountOffset(uid, "checkin"); got != first {
			t.Fatalf("偏移不稳定：第 %d 次 %v != 首次 %v", i, got, first)
		}
	}
}

// TestAccountOffsetInRange 偏移必须落在 [0, AccountSpread)。
func TestAccountOffsetInRange(t *testing.T) {
	spread := 4 * time.Minute
	s := &Scheduler{cfg: spreadCfg()}
	uids := []string{
		"0c33a277", "20adac3a", "3bc5407f", "721bdff9",
		"a5a9a67d", "b9032a06", "ccfb20aa",
	}
	for _, uid := range uids {
		for _, kind := range []string{"checkin", "travel", "activity"} {
			got := s.accountOffset(uid, kind)
			if got < 0 || got >= spread {
				t.Errorf("%s/%s 偏移 %v 越界 [0,%v)", uid, kind, got, spread)
			}
		}
	}
}

// TestAccountOffsetDesynchronizes 7 个号的偏移不能全一样——
// 全一样就等于没铺开，这是本次改造要消灭的「整组同步」。
func TestAccountOffsetDesynchronizes(t *testing.T) {
	s := &Scheduler{cfg: spreadCfg()}
	uids := []string{
		"0c33a277", "20adac3a", "3bc5407f", "721bdff9",
		"a5a9a67d", "b9032a06", "ccfb20aa",
	}
	seen := map[time.Duration]int{}
	for _, uid := range uids {
		seen[s.accountOffset(uid, "checkin")]++
	}
	if len(seen) < 2 {
		t.Fatalf("7 个号只落到 %d 个不同时刻：%v", len(seen), seen)
	}
}

// TestAccountOffsetVariesByTaskFamily 任务族参与哈希：签到/旅行/活跃不能共享同一
// 批偏移时刻，否则仍能从时间轴看出「同一时刻四个动作」的规整形状。
func TestAccountOffsetVariesByTaskFamily(t *testing.T) {
	s := &Scheduler{cfg: spreadCfg()}
	diff := 0
	for _, uid := range []string{"0c33a277", "20adac3a", "3bc5407f", "721bdff9", "a5a9a67d"} {
		if s.accountOffset(uid, "checkin") != s.accountOffset(uid, "travel") {
			diff++
		}
	}
	if diff < 3 {
		t.Errorf("签到/旅行偏移不同的号只有 %d/5，任务族区分度不足", diff)
	}
}

// TestSpreadDisabledIsZero 铺开关闭时偏移/抖动/间隔全部零值，行为与引入前逐字一致。
func TestSpreadDisabledIsZero(t *testing.T) {
	s := &Scheduler{cfg: Config{}} // AccountSpread=0 → 关闭
	if got := s.accountOffset("0c33a277", "checkin"); got != 0 {
		t.Errorf("关闭时偏移应为 0，实际 %v", got)
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
	s := &Scheduler{cfg: spreadCfg()}
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
	s := &Scheduler{cfg: spreadCfg()}
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
	s := &Scheduler{cfg: spreadCfg()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.waitAccountTurn(ctx, "0c33a277", "checkin") {
		t.Error("ctx 已取消时 waitAccountTurn 应返回 false")
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

// newSpreadTestPool 建 n 个带凭证的账号（batchReserve 要按池大小估算单号开销）。
func newSpreadTestPool(t *testing.T, n int) *pool.Pool {
	t.Helper()
	p := pool.New("")
	for i := 0; i < n; i++ {
		p.Add(&auth.Auth{
			UID:         fmt.Sprintf("uid-%d", i),
			AccessToken: "at",
			RefreshToken: "rt",
			ExpiresAt:   9999999999,
		})
	}
	return p
}

// TestBatchReserveShrinksRandomWindow 随机窗口基准必须预留出一整批的时长，
// 否则批尾会落进夜猫窗口（23:00 起）把夜猫子时点挤掉——
// 等于用「反指纹」换来「丢一个任务」。
func TestBatchReserveShrinksRandomWindow(t *testing.T) {
	cfg := spreadCfg()
	cfg.Pool = newSpreadTestPool(t, 7)
	cfg.RandomWindowStartMin = 8*60 + 33  // 08:33
	cfg.RandomWindowEndMin = 23*60 + 25 // 23:25
	s := New(cfg)

	if s.batchReserve() <= 0 {
		t.Fatalf("开启铺开后 batchReserve 应 >0，实际 %v", s.batchReserve())
	}
	limit := time.Date(2026, 10, 4, 23, 25, 0, 0, cstZone)
	start := time.Date(2026, 10, 4, 8, 33, 0, 0, cstZone)
	// 抽样 500 次：基准时刻 + 预留 必须仍在窗口内。
	for i := 0; i < 500; i++ {
		at := s.randomInWindow("2026-10-04")
		if at.Before(start) || at.Add(s.batchReserve()).After(limit) {
			t.Fatalf("基准 %s + 预留 %v 越出随机窗口 [08:33, 23:25]",
				at.Format("15:04"), s.batchReserve())
		}
	}
}

// TestBatchReserveDisabledIsZero 关闭铺开时不预留，随机窗口行为与引入前逐字一致。
func TestBatchReserveDisabledIsZero(t *testing.T) {
	s := &Scheduler{cfg: Config{}}
	if got := s.batchReserve(); got != 0 {
		t.Errorf("关闭铺开时 batchReserve 应为 0，实际 %v", got)
	}
}

// TestRandDelayBounds randDelay 必须落在 [min,max] 且倒置时退化为 min。
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
