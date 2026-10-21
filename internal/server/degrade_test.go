package server

import (
	"testing"
	"time"
)

// TestNextMidnightCSTBoundaries 边界：CST 视角下当日 00:00 之后 → 次日 00:00，
// 23:59 → 次日 00:00，正午 → 次日 00:00。用固定 +08:00 偏移计算，不依赖系统时区。
func TestNextMidnightCSTBoundaries(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		name string
		now  time.Time
		// 期望 until 的 CST 时分秒恒为 00:00:00，且 strictly after now。
	}{
		{"23:59 -> next 00:00", time.Date(2026, 9, 11, 23, 59, 0, 0, cst)},
		{"00:00:01 -> next 00:00", time.Date(2026, 9, 11, 0, 0, 1, 0, cst)},
		{"12:00 -> next 00:00", time.Date(2026, 9, 11, 12, 0, 0, 0, cst)},
		{"00:00:00 exact -> next 00:00", time.Date(2026, 9, 11, 0, 0, 0, 0, cst)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextMidnightCST(tc.now)
			if !got.After(tc.now) {
				t.Fatalf("until %v not after now %v", got, tc.now)
			}
			// CST 视角下必须落在 00:00:00。
			if g := got.In(cst); g.Hour() != 0 || g.Minute() != 0 || g.Second() != 0 {
				t.Errorf("until CST = %02d:%02d:%02d, want 00:00:00", g.Hour(), g.Minute(), g.Second())
			}
		})
	}
}

// TestNextMidnightCSTCrossMonth 跨月：月末 23:59 → 次月 1 日 00:00。
func TestNextMidnightCSTCrossMonth(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 1, 31, 23, 59, 0, 0, cst)
	got := nextMidnightCST(now)
	want := time.Date(2026, 2, 1, 0, 0, 0, 0, cst)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestDegradeGateActiveTriggered ActiveFor() 在 TriggerFor 后为 true（仅该指纹）。
func TestDegradeGateActiveTriggered(t *testing.T) {
	var g degradeGate
	if g.ActiveFor("k1") {
		t.Error("fresh gate should not be active")
	}
	g.TriggerFor("k1")
	if !g.ActiveFor("k1") {
		t.Error("after TriggerFor should be active")
	}
}

// TestDegradeGateTriggerNoRenewal 降级期内再次 TriggerFor 不续期（保持最早触发点的 00:00 重置）。
func TestDegradeGateTriggerNoRenewal(t *testing.T) {
	var g degradeGate
	g.TriggerFor("k1")
	first := g.Deadline("k1")
	// 再次 TriggerFor：仍在降级期内，until 不应改变。
	g.TriggerFor("k1")
	second := g.Deadline("k1")
	if !first.Equal(second) {
		t.Errorf("TriggerFor during active should not renew: first=%v second=%v", first, second)
	}
}

// TestDegradeGateScopedByFingerprint 2026-10-21 事故的回归锚：降级只能影响
// **撞过墙的那一个 system 指纹**，别的客户端（如 tian_yan 的【标的】事实块）逐字不动。
//
// 事故现场：passthrough 全局降级 → tian_yan 的 system（含【标的】云中马 603130）
// 被换成 Degraded 中性提示词，模型反问"哪家公司"。
func TestDegradeGateScopedByFingerprint(t *testing.T) {
	var g degradeGate
	g.TriggerFor("clientA")
	if !g.ActiveFor("clientA") {
		t.Fatal("triggered fingerprint should be active")
	}
	if g.ActiveFor("clientB") {
		t.Error("other fingerprints must NOT be degraded by someone else's block")
	}
	if g.ActiveFor("") {
		t.Error("empty fingerprint (no client system) must never be degraded")
	}
}

// TestDegradeGateExpiry 过期后 ActiveFor 自动失效并清掉条目。
func TestDegradeGateExpiry(t *testing.T) {
	var g degradeGate
	g.TriggerFor("k1")
	// 人为把截止时刻拨到过去（模拟跨过 00:00）。
	g.mu.Lock()
	g.state["k1"].until = time.Now().Add(-time.Second)
	g.mu.Unlock()
	if g.ActiveFor("k1") {
		t.Error("expired entry should not be active")
	}
	g.mu.Lock()
	_, still := g.state["k1"]
	g.mu.Unlock()
	if still {
		t.Error("expired entry should be pruned")
	}
}

// TestDegradeGateShouldLogApplyOnce 每个指纹每次降级期只打一行 apply 日志。
func TestDegradeGateShouldLogApplyOnce(t *testing.T) {
	var g degradeGate
	g.TriggerFor("k1")
	if !g.ShouldLogApply("k1") {
		t.Error("first apply should log")
	}
	if g.ShouldLogApply("k1") {
		t.Error("second apply in same window should NOT log again")
	}
	if g.ShouldLogApply("unknown") {
		t.Error("untriggered fingerprint should not log")
	}
}
