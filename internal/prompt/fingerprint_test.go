package prompt

import "testing"

// TestSystemFingerprintStable 同一段 system 文本 → 同一指纹（JSON 键序/空白不影响）。
func TestSystemFingerprintStable(t *testing.T) {
	a := SystemFingerprint([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":"事实块"}]}`))
	b := SystemFingerprint([]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":"事实块"}],"model":"m"}`))
	if a == "" {
		t.Fatal("fingerprint should not be empty")
	}
	if a != b {
		t.Errorf("fingerprint should be stable: %q vs %q", a, b)
	}
}

// TestSystemFingerprintDistinguishesClients 不同客户端的 system → 不同指纹
// （2026-10-21 事故的核心判据：A 撞墙不能把 B 的 system 一起换掉）。
func TestSystemFingerprintDistinguishesClients(t *testing.T) {
	cc := SystemFingerprint([]byte(`{"messages":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI tool."},{"role":"user","content":"hi"}]}`))
	ty := SystemFingerprint([]byte(`{"messages":[{"role":"system","content":"以下是这只票当前的事实：\n【标的】云中马 603130 · 板块：机器人"},{"role":"user","content":"hi"}]}`))
	if cc == "" || ty == "" {
		t.Fatal("both fingerprints should be non-empty")
	}
	if cc == ty {
		t.Error("different client systems must not share a fingerprint")
	}
}

// TestSystemFingerprintNoSystem 没有 system/developer → 空串（调用方据此不参与降级）。
func TestSystemFingerprintNoSystem(t *testing.T) {
	cases := map[string]string{
		"只有 user":  `{"messages":[{"role":"user","content":"hi"}]}`,
		"空 messages": `{"messages":[]}`,
		"无 messages": `{"model":"m"}`,
		"坏 JSON":    `{not json`,
		"空 body":    ``,
	}
	for name, body := range cases {
		if got := SystemFingerprint([]byte(body)); got != "" {
			t.Errorf("%s: want empty fingerprint, got %q", name, got)
		}
	}
}

// TestSystemFingerprintBlockContent content 为块数组时取 text 段（Anthropic 形态的入站体）。
func TestSystemFingerprintBlockContent(t *testing.T) {
	blocks := SystemFingerprint([]byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"事实块"},{"type":"image","source":{}}]}]}`))
	plain := SystemFingerprint([]byte(`{"messages":[{"role":"system","content":"事实块"}]}`))
	if blocks == "" {
		t.Fatal("block content should produce a fingerprint")
	}
	if blocks != plain {
		t.Errorf("text blocks should fingerprint like the equivalent plain string: %q vs %q", blocks, plain)
	}
}

// TestSystemFingerprintDeveloperRoleCounts developer 是 OpenAI 口径里的 system 别名，
// 归一在出站前才做，指纹阶段必须一并计入（否则 append/降级判定对不上）。
func TestSystemFingerprintDeveloperRoleCounts(t *testing.T) {
	dev := SystemFingerprint([]byte(`{"messages":[{"role":"developer","content":"客户端规范"}]}`))
	sys := SystemFingerprint([]byte(`{"messages":[{"role":"system","content":"客户端规范"}]}`))
	if dev == "" {
		t.Fatal("developer role should produce a fingerprint")
	}
	if dev != sys {
		t.Errorf("developer and system should be treated identically: %q vs %q", dev, sys)
	}
}

// TestSystemFingerprintChangesWithContent 改一个字 → 换指纹（防"降级期把新事实块也吃掉"）。
func TestSystemFingerprintChangesWithContent(t *testing.T) {
	a := SystemFingerprint([]byte(`{"messages":[{"role":"system","content":"【标的】云中马 603130"}]}`))
	b := SystemFingerprint([]byte(`{"messages":[{"role":"system","content":"【标的】云中马 603131"}]}`))
	if a == b {
		t.Error("different system texts must produce different fingerprints")
	}
}
