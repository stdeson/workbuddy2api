package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestBanStormGateSingleAccountNeverTriggers 单号反复 11140 永不触发风暴闸门
// ——11140 天然是账号级，单个号被判异常完全可能，不能因此停服。
func TestBanStormGateSingleAccountNeverTriggers(t *testing.T) {
	var g banStormGate
	for i := 0; i < 10; i++ {
		if g.noteBan("u1") {
			t.Fatalf("single account must never trigger fleet-level gate (iter %d)", i)
		}
	}
	if g.active() {
		t.Fatal("gate must not be active")
	}
}

// TestBanStormGateTriggersOnDistinctAccounts 短窗内多个**不同**账号命中 11140 →
// 闸门激活（这是 2026-10-29 集体封禁的形态）。
func TestBanStormGateTriggersOnDistinctAccounts(t *testing.T) {
	var g banStormGate
	if g.noteBan("u1") || g.noteBan("u2") {
		t.Fatal("gate must not trigger below threshold")
	}
	if !g.noteBan("u3") {
		t.Fatal("gate must trigger at threshold (3 distinct accounts)")
	}
	if !g.active() {
		t.Fatal("gate must be active after triggering")
	}
}

// TestBanStormGateSingleHitPerAccountDoesNotCount 多号计数按**不同账号**数：
// 同一个账号反复命中只算 1 个号。
func TestBanStormGateSingleHitPerAccountDoesNotCount(t *testing.T) {
	var g banStormGate
	for i := 0; i < 20; i++ {
		if g.noteBan("u1") {
			t.Fatalf("same account repeated must not trigger (iter %d)", i)
		}
	}
	if g.active() {
		t.Fatal("gate must not be active")
	}
}

// TestBanStormGateExpires 激活到期后自然解除（需全新命中重新判定）。
func TestBanStormGateExpires(t *testing.T) {
	old := banStormWindow
	banStormWindow = 50 * time.Millisecond
	defer func() { banStormWindow = old }()

	var g banStormGate
	g.noteBan("u1")
	g.noteBan("u2")
	if !g.noteBan("u3") {
		t.Fatal("gate must trigger")
	}
	time.Sleep(80 * time.Millisecond)
	if g.active() {
		t.Fatal("gate must expire naturally")
	}
}

// TestBanStormStopsRotation 端到端：池里 4 个号，前 3 个都返回 11140。
// 期望：**第 4 个号不被选中**（轮转在第 3 次命中后被闸门终止）——
// 这正是 2026-10-29「86 秒烧光 7 个号」的止损点。
func TestBanStormStopsRotation(t *testing.T) {
	old := banStormWindow
	banStormWindow = 60 * time.Second
	defer func() { banStormWindow = old }()

	var calls atomic.Int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 403, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u4", AccessToken: "at4", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 5, SoftCooldown: 90 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	// 命中 3 个不同号后闸门激活，第 4 个号必须没被碰过。
	if n := calls.Load(); n > 3 {
		t.Fatalf("upstream calls=%d want <=3 (rotation must stop at ban storm threshold)", n)
	}
	if !h.banStorm.active() {
		t.Fatal("ban storm gate must be active after 3 distinct 11140 hits")
	}
	// 已撞封的 3 个号各自进封禁冷却。
	for _, uid := range []string{"u1", "u2", "u3"} {
		st, _ := p.Status(uid)
		if st.Disabled {
			t.Errorf("%s must not be permanently disabled: %+v", uid, st)
		}
		if !st.Cooling || st.CoolKind != "banned_11140" {
			t.Errorf("%s must be in banned cooldown: %+v", uid, st)
		}
	}
}

// TestBanStormGateBlocksNextRequest 闸门激活后，后续请求**不打上游**直接 fail-fast。
func TestBanStormGateBlocksNextRequest(t *testing.T) {
	old := banStormWindow
	banStormWindow = 60 * time.Second
	defer func() { banStormWindow = old }()

	var calls atomic.Int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 403, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u4", AccessToken: "at4", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 5, SoftCooldown: 90 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	before := calls.Load()

	// 闸门已激活（第一个请求就该激活：3 个不同号）。
	if !h.banStorm.active() {
		t.Fatal("gate must be active")
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if after := calls.Load(); after != before {
		t.Fatalf("gated request must not hit upstream: calls %d -> %d", before, after)
	}
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("gated request code=%d want 503", rec2.Code)
	}
}

// TestIsBannedFaultMatchesOnly11140 isBannedFault 判据：只认 "request illegal"
// （11140），不误伤其他账号级故障（14017 trial）。
func TestIsBannedFaultMatchesOnly11140(t *testing.T) {
	if !isBannedFault(`{"error":{"data":{"code":11140,"msg":"request illegal"}}}`) {
		t.Error("11140 request illegal must match")
	}
	if !isBannedFault(`{"code":11140,"msg":"REQUEST ILLEGAL"}`) {
		t.Error("match must be case-insensitive")
	}
	if isBannedFault(`{"code":14017,"msg":"The trial version is not yet activated"}`) {
		t.Error("14017 must not match")
	}
	if isBannedFault(`{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`) {
		t.Error("11128 must not match")
	}
}

// TestBannedAccountNotInFallbackPool 封禁冷却的号不进「全冷却兜底」——
// 封禁期打上游必然再撞 11140，只会持续触发风控。
func TestBannedAccountNotInFallbackPool(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.CooldownBanned("u1", "11140 request illegal")
	if got := p.Pick(""); got != nil {
		t.Fatalf("banned account must not be picked (even via fallback): %+v", got)
	}
}

// TestErrAccountFaultHintDoesNotPromiseDisable hint 文案不得承诺「禁用」——
// 11140 现在走封禁冷却（可自动恢复），不是永久禁用。
func TestErrAccountFaultHintDoesNotPromiseDisable(t *testing.T) {
	hint := upstream.GatewayHint(upstream.ErrAccountFault, `{"code":11140,"msg":"request illegal"}`, upstream.HintContext{})
	if strings.Contains(hint, "disable") {
		t.Fatalf("hint must not promise permanent disable: %q", hint)
	}
}
