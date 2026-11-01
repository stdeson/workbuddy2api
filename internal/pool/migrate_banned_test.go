package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestMigrateLegacyBannedDisabled 旧 11140 永久禁用的账号被解除禁用、重新可选——
// 否则「修复上线了但账号还是死的」：不被选中 → 不触发新 11140 → 永远不恢复。
func TestMigrateLegacyBannedDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})

	p.Disable("u1", legacyBannedReason)
	p.Disable("u2", "12153 session dead") // 其它禁用原因 → 不该被迁

	if n := p.MigrateLegacyBannedDisabled(); n != 1 {
		t.Fatalf("migrated=%d want 1", n)
	}
	st1, _ := p.Status("u1")
	if st1.Disabled {
		t.Fatalf("legacy 11140 account must be re-enabled: %+v", st1)
	}
	st2, _ := p.Status("u2")
	if !st2.Disabled {
		t.Fatalf("non-11140 disable must be untouched: %+v", st2)
	}
	if got := p.Pick(""); got == nil {
		t.Fatal("migrated account must be selectable again")
	}
}

// TestMigrateLegacyBannedDisabledIdempotent 迁移可重复执行（每次启动都跑）：
// 第二次不应再迁（已经是可恢复状态）。
func TestMigrateLegacyBannedDisabledIdempotent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Disable("u1", legacyBannedReason)

	if n := p.MigrateLegacyBannedDisabled(); n != 1 {
		t.Fatalf("first migration=%d want 1", n)
	}
	if n := p.MigrateLegacyBannedDisabled(); n != 0 {
		t.Fatalf("second migration=%d want 0 (idempotent)", n)
	}
}

// TestMigrateLegacyBannedDisabledPersistsAndReloads 迁移结果落盘并能在重启后保持
// （重启后账号仍是可恢复状态，不会被旧 state.json 拖回死状态）。
func TestMigrateLegacyBannedDisabledPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Disable("u1", legacyBannedReason)
	p.MigrateLegacyBannedDisabled()

	p.mu.Lock()
	p.saveLocked()
	p.mu.Unlock()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "at2"})
	p2.mu.Lock()
	p2.load()
	p2.mu.Unlock()

	st, _ := p2.Status("u1")
	if st.Disabled {
		t.Fatalf("reloaded account must stay recoverable: %+v", st)
	}
}
