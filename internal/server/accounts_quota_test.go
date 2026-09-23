package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// resetQuotaCache 清空包级配额缓存（TTL 30s，测试间必须复位，否则互相污染）。
func resetQuotaCache() {
	quotaCache.Lock()
	quotaCache.rows = nil
	quotaCache.at = time.Time{}
	quotaCache.Unlock()
}

// quotaBody 一份 get-user-resource 响应（含一个周期包 + 一个一次性赠送包）。
const quotaBody = `{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
	`{"PackageName":"周期包","CycleEndTime":"2026-10-01 00:00:00","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200},` +
	`{"PackageName":"赠送包","CapacitySize":100,"CapacityRemain":40,"CapacityUsed":60}` +
	`]}}}}`

// TestAccountsEndpointListsPinnedPaths：/v1/accounts 列出每个池成员的固定号端点路径，
// 冷却账号带 cool_until/cool_remaining（看板直接渲染，无需前端再算）。
func TestAccountsEndpointListsPinnedPaths(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999, Nickname: "甲"},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	p.Cooldown("u2", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, quotaBody, false
	})})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Provider string `json:"provider"`
		Accounts []struct {
			UID       string `json:"uid"`
			Nickname  string `json:"nickname"`
			Credits   int64  `json:"credits"`
			Cooling   bool   `json:"cooling"`
			ChatPath  string `json:"chat_path"`
			QuotaPath string `json:"quota_path"`
			CoolUntil string `json:"cool_until"`
			Remaining string `json:"cool_remaining"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	if got.Provider != "workbuddy" {
		t.Errorf("provider=%q", got.Provider)
	}
	if len(got.Accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(got.Accounts))
	}
	byUID := map[string]int{}
	for i, a := range got.Accounts {
		byUID[a.UID] = i
		if a.ChatPath != "/v1/a/"+a.UID+"/chat/completions" {
			t.Errorf("%s chat_path=%q", a.UID, a.ChatPath)
		}
		if a.QuotaPath != "/v1/a/"+a.UID+"/quota" {
			t.Errorf("%s quota_path=%q", a.UID, a.QuotaPath)
		}
	}
	i2, ok := byUID["u2"]
	if !ok {
		t.Fatal("u2 未出现在账号列表")
	}
	if !got.Accounts[i2].Cooling || got.Accounts[i2].CoolUntil == "" || got.Accounts[i2].Remaining == "" {
		t.Errorf("冷却账号应带冷却信息: %+v", got.Accounts[i2])
	}
	if i1 := byUID["u1"]; got.Accounts[i1].Nickname != "甲" {
		t.Errorf("u1 昵称=%q want 甲", got.Accounts[i1].Nickname)
	}
}

// TestQuotaEndpointsProbePackages：/v1/quota 返回全池套餐明细；固定号 quota 只回一个账号；
// 未知 uid 404。冷却账号跳过探测（error 说明原因，而不是把"没探到"渲染成 0）。
func TestQuotaEndpointsProbePackages(t *testing.T) {
	resetQuotaCache()
	t.Cleanup(resetQuotaCache)
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	p.Cooldown("u2", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, quotaBody, false
	})})

	type quotaRow struct {
		UID    string `json:"uid"`
		Error  string `json:"error"`
		Quotas map[string]struct {
			PackageName string `json:"packageName"`
			Total       int64  `json:"total"`
			Used        int64  `json:"used"`
			Remaining   int64  `json:"remaining"`
			ResetAt     string `json:"resetAt"`
			Recurring   bool   `json:"recurring"`
		} `json:"quotas"`
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) []quotaRow {
		t.Helper()
		var got struct {
			Accounts []quotaRow `json:"accounts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body)
		}
		return got.Accounts
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/quota", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	rows := decode(t, rec)
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	var u1, u2 *quotaRow
	for i := range rows {
		switch rows[i].UID {
		case "u1":
			u1 = &rows[i]
		case "u2":
			u2 = &rows[i]
		}
	}
	if u1 == nil || u2 == nil {
		t.Fatalf("缺账号行: %+v", rows)
	}
	pkg, ok := u1.Quotas["周期包"]
	if !ok {
		t.Fatalf("u1 缺周期包: %+v", u1.Quotas)
	}
	if pkg.Total != 500 || pkg.Used != 200 || pkg.Remaining != 300 || !pkg.Recurring {
		t.Errorf("周期包字段错: %+v", pkg)
	}
	if pkg.ResetAt != "2026-10-01 00:00:00" {
		t.Errorf("resetAt=%q", pkg.ResetAt)
	}
	if _, ok := u1.Quotas["赠送包"]; !ok {
		t.Error("赠送包不应被过滤掉（本口径不带 ProductCode/Status 过滤）")
	}
	if u2.Quotas != nil || u2.Error == "" {
		t.Errorf("冷却账号应跳过探测并给出原因: %+v", u2)
	}

	// 固定号 quota：只回一个账号（复用同一份缓存，不再触发全池探测）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/a/u1/quota", nil))
	if rec.Code != 200 {
		t.Fatalf("pinned code=%d body=%s", rec.Code, rec.Body)
	}
	if rows := decode(t, rec); len(rows) != 1 || rows[0].UID != "u1" {
		t.Errorf("固定号 quota=%+v want 单行 u1", rows)
	}

	// 未知 uid：404 not_found（不能静默落到全池结果）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/a/nope/quota", nil))
	if rec.Code != 404 {
		t.Fatalf("未知 uid code=%d want 404", rec.Code)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "not_found") {
		t.Errorf("错误信封不符: %s", rec.Body)
	}
}

// TestPinnedChatUsesPinnedAccount：/v1/a/{uid}/chat/completions 必须用**指定账号**，
// 而不是池化轮换选中的号；账号不存在时 404 且不打上游。
func TestPinnedChatUsesPinnedAccount(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		seen = append(seen, authz)
		mu.Unlock()
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	// 确定性随机源（恒 0）+ u1 积分更高 → 普通轮换必选 u1；
	// 因此"选了 u2"只可能来自 pin。
	p.SetCredits("u1", 2000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	body := []byte(`{"model":"glm-5.2","messages":[],"stream":true}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/a/u2/chat/completions", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("pinned chat code=%d body=%s", rec.Code, rec.Body)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "Bearer at2" {
		t.Fatalf("上游收到 %v want [Bearer at2]（pin 未生效则会用加权选中的 u1）", got)
	}

	// 未知 uid：404，且不再打上游。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/a/nope/chat/completions", bytes.NewReader(body)))
	if rec.Code != 404 {
		t.Fatalf("未知 uid code=%d want 404 body=%s", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "not_found") {
		t.Errorf("错误信封不符: %s", rec.Body)
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 1 {
		t.Errorf("未知 uid 不应打上游，上游调用数=%d", n)
	}
}

// TestQuotaProbeFailureSurfacesError：上游探测失败时必须把原因透出（error 非空），
// 不能渲染成"这个号没有套餐"。
func TestQuotaProbeFailureSurfacesError(t *testing.T) {
	resetQuotaCache()
	t.Cleanup(resetQuotaCache)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":11101,"msg":"Unmarshal chat params failed"}`, false
	})})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/quota", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Accounts []struct {
			Error string `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].Error == "" {
		t.Errorf("探测失败应带 error: %+v", got.Accounts)
	}
}
