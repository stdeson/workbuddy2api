package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestBannedCooldownNotPermanentDisable 11140 封禁冷却**不是**永久禁用：
// 账号进入 banned_11140 冷却态（不进 Disabled 位），冷却到期后自动回到候选池。
//
// 背景（2026-11-01 实测）：旧实现对 11140 直接 Disable（永久摘号），7 个号里 6 个
// 在 10-29 被摘除后一个都没自动恢复，池容量塌缩到 1 个号；而上游封禁其实会自愈。
func TestBannedCooldownNotPermanentDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	p.CooldownBanned("u1", "11140 request illegal")

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if st.Disabled {
		t.Fatalf("11140 must not permanently disable: %+v", st)
	}
	if !st.Cooling {
		t.Fatalf("11140 must enter cooldown: %+v", st)
	}
	if st.CoolKind != "banned_11140" {
		t.Fatalf("cool_kind=%q want banned_11140: %+v", st.CoolKind, st)
	}
	if st.CoolRemaining <= 0 {
		t.Fatalf("banned cooldown must have positive remaining: %+v", st)
	}

	// 冷却期间不可选（不刷上游风控）。
	if got := p.Pick(""); got != nil {
		t.Fatalf("cooling account must not be picked: %+v", got)
	}
}

// TestBannedCooldownAutoHalfOpen 核心新行为：冷却到期后账号**自动**回到候选池
// （半开探测）——这是「永久禁用」与「长冷却」的本质差别。
func TestBannedCooldownAutoHalfOpen(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	p.CooldownBanned("u1", "11140 request illegal")

	// 模拟冷却到期（直接推进 until，避免测试真等 6h）。
	p.mu.Lock()
	p.byUID["u1"].until = time.Now().Add(-time.Minute)
	p.mu.Unlock()

	got := p.Pick("")
	if got == nil || got.UID != "u1" {
		t.Fatalf("expired banned cooldown must return to pool (half-open probe): %+v", got)
	}
}

// TestBannedCooldownBackoffDoubles 连续封禁 → 冷却时长按 streak 翻倍（封顶 48h），
// 让「持续封禁」的号安静下来，把在途名额让给健康号。
func TestBannedCooldownBackoffDoubles(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	var prev time.Duration
	for i := 1; i <= 4; i++ {
		p.CooldownBanned("u1", "11140 request illegal")
		rem := p.BannedCooldownRemaining("u1")
		if rem <= 0 {
			t.Fatalf("iter %d: remaining=%v want >0", i, rem)
		}
		if prev > 0 && rem < prev {
			t.Fatalf("iter %d: cooldown must not shrink: prev=%v now=%v", i, prev, rem)
		}
		if rem > bannedCooldownMax+time.Minute {
			t.Fatalf("iter %d: remaining=%v exceeds cap %v", i, rem, bannedCooldownMax)
		}
		prev = rem
	}
	// 第 4 次应是 6h*2^3 = 48h（= 封顶）。
	if prev < bannedCooldownMax-time.Minute {
		t.Fatalf("after 4 bans cooldown=%v want ~%v (cap)", prev, bannedCooldownMax)
	}
}

// TestBannedStreakResetOnSuccess 半开探测成功 → bannedStreak 清零，
// 下一次封禁从 base（6h）重新起退避，而不是接着上次的翻倍加长。
func TestBannedStreakResetOnSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	p.CooldownBanned("u1", "11140 request illegal")
	p.CooldownBanned("u1", "11140 request illegal") // streak=2 → 12h

	p.NoteSuccess("u1") // 半开探测成功

	p.mu.Lock()
	streak := p.byUID["u1"].bannedStreak
	p.mu.Unlock()
	if streak != 0 {
		t.Fatalf("banned_streak=%d want 0 after successful half-open probe", streak)
	}

	p.CooldownBanned("u1", "11140 request illegal")
	rem := p.BannedCooldownRemaining("u1")
	// 回到 base 6h（不是 12h / 24h）。
	if rem < bannedCooldownBase-time.Minute || rem > bannedCooldownBase+time.Minute {
		t.Fatalf("remaining=%v want ~%v (base after reset)", rem, bannedCooldownBase)
	}
}

// TestBannedStreakPersisted bannedStreak 落盘：重启后封禁退避不归零，
// 否则每次重启都会把被封号拉回 base 冷却试探，形成重启即试探的风暴。
func TestBannedStreakPersisted(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.CooldownBanned("u1", "11140 request illegal")
	p.CooldownBanned("u1", "11140 request illegal")

	p.mu.Lock()
	p.saveLocked()
	p.mu.Unlock()

	// 重启恢复。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.Lock()
	p2.load()
	p2.mu.Unlock()

	p2.mu.Lock()
	streak := p2.byUID["u1"].bannedStreak
	p2.mu.Unlock()
	if streak != 2 {
		t.Fatalf("banned_streak after reload=%d want 2", streak)
	}
}

// TestBannedCooldownKeepsManualDisabled 封禁冷却**不动**手动停用位：
// 运维明确摘除的号不应被一次 11140 半开探测悄悄放回选号池。
func TestBannedCooldownKeepsManualDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	p.SetManualDisabled("u1", true, "manual")
	p.CooldownBanned("u1", "11140 request illegal")

	st, _ := p.Status("u1")
	if !st.ManualDisabled {
		t.Fatalf("manual_disabled must be untouched by banned cooldown: %+v", st)
	}
	if got := p.Pick(""); got != nil {
		t.Fatalf("manually disabled account must not be picked: %+v", got)
	}
}
