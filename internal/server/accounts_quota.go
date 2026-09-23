// accounts_quota.go 固定号端点族：GET /v1/accounts、GET /v1/quota、GET /v1/a/{uid}/quota。
//
// 面向的上游网关（9Router 等）不是「所有人共享一个池化连接」，而是**按账号各建一条连接**
// 自行轮转；这三条只读端点就是给它们发现账号、读各自积分的：
//   - /v1/accounts 列出每个池成员及其固定号端点路径（chat_path / quota_path）；
//   - /v1/quota 一次性给出全池积分套餐（带短 TTL 缓存，避免看板轮询打爆上游 billing）；
//   - /v1/a/{uid}/quota 只回一个账号，复用同一份缓存——否则 N 个连接 × N 个账号 = N² 次探测。
//
// 来源：社区 fork 287775856/linbeize（workbuddy2api）的 handler 实现，按本仓库的
// pool.Status（含 realm / cool_kind / 成功失败计数等本仓库独有字段）与
// upstream.ResourcePackages 重写；固定号转发（/v1/a/{uid}/chat/completions）在
// handler.go 的 chatCompletions 内以 pin 语义实现。
package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"workbuddy2api/internal/upstream"
)

// pinnedChat 处理 POST /v1/a/{uid}/chat/completions：把请求钉到指定账号。
//
// 真正的钉号逻辑在 chatCompletions 内（r.PathValue("uid") → stickyUID，但**不写**
// 会话绑定）；这里只做存在性与前置校验，让"账号不存在"明确返回 404 而不是
// 落到池化轮换上去（否则调用方会以为钉号生效了，实际请求被别人服务）。
func (h *Handler) pinnedChat(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing account uid")
		return
	}
	if h.cfg.Pool.AuthByUID(uid) == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	h.chatCompletions(w, r)
}

// accounts 处理 GET /v1/accounts：列出池内每个账号 + 它的固定号端点路径。
//
// 只读快照（Pool.List 不触发任何上游调用），因此可以放心高频轮询。
// realm 字段是本仓库扩展（多域账号池）：上游网关据此把 CN / global 分到不同连接池。
func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	list := h.cfg.Pool.List()
	type acct struct {
		UID       string `json:"uid"`
		Realm     string `json:"realm,omitempty"`
		Nickname  string `json:"nickname,omitempty"`
		Credits   int64  `json:"credits"`
		Cooling   bool   `json:"cooling"`
		Disabled  bool   `json:"disabled"`
		ChatPath  string `json:"chat_path"`
		QuotaPath string `json:"quota_path"`
		CoolUntil string `json:"cool_until,omitempty"`
		Remaining string `json:"cool_remaining,omitempty"`
	}
	out := make([]acct, 0, len(list))
	for _, st := range list {
		a := acct{
			UID:       st.UID,
			Realm:     st.Realm,
			Nickname:  st.Nickname,
			Credits:   st.Credits,
			Cooling:   st.Cooling,
			Disabled:  st.Disabled,
			ChatPath:  "/v1/a/" + st.UID + "/chat/completions",
			QuotaPath: "/v1/a/" + st.UID + "/quota",
		}
		if st.Cooling && !st.Until.IsZero() {
			a.CoolUntil = st.Until.Format(time.RFC3339)
			a.Remaining = formatRemaining(time.Until(st.Until))
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": "workbuddy", "accounts": out})
}

// quota 处理 GET /v1/quota：全池账号的积分套餐（9Router 看板 quota 形状）。
func (h *Handler) quota(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": "workbuddy",
		"accounts": h.cachedAccountQuotas(),
	})
}

// pinnedQuota 处理 GET /v1/a/{uid}/quota：只回该账号一行。
func (h *Handler) pinnedQuota(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing account uid")
		return
	}
	// 复用缓存的全量结果再过滤，避免每个 per-account 连接都触发一轮全池探测。
	for _, a := range h.cachedAccountQuotas() {
		if a.UID == uid {
			writeJSON(w, http.StatusOK, map[string]any{
				"provider": "workbuddy",
				"accounts": []accountQuota{a},
			})
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
}

// quotaProbeTimeout 单次配额探测的总预算：所有账号探测共享这一个 deadline。
//
// 为什么需要总预算：上游单次调用超时是 120s（Client.HTTP.Timeout），若按账号串行探测，
// N 个账号最坏要 N×120s（46 账号 ≈ 92 分钟），请求早被客户端/反代掐断，连接与上游配额
// 也白白占用。这里给整体探测一个上限并**并发**执行，超时账号按"未知"返回。
const quotaProbeTimeout = 15 * time.Second

// quotaCacheTTL /v1/quota 结果的缓存时长。
//
// 为什么必须缓存：配额看板通常按秒级轮询，而每次调用都会向上游逐个账号打 billing 接口。
// 无缓存时多账号 + 高频轮询会直接触发上游限流。缓存一份短 TTL 结果，既保证数据接近实时，
// 又把上游调用量压到「每 TTL 一次」。
const quotaCacheTTL = 30 * time.Second

// quotaCache 缓存上一次配额探测结果。
//
// 进程级单例（非 Handler 字段）：本仓库 Handler 单实例，且探测结果与实例无关；
// 放包级变量的好处是多个端点（/v1/quota 与 /v1/a/{uid}/quota）天然共享同一份缓存。
var quotaCache struct {
	sync.RWMutex
	rows []accountQuota
	at   time.Time
}

// cachedAccountQuotas 返回缓存结果；缓存过期或为空时重新探测。
func (h *Handler) cachedAccountQuotas() []accountQuota {
	quotaCache.RLock()
	if len(quotaCache.rows) > 0 && time.Since(quotaCache.at) < quotaCacheTTL {
		rows := quotaCache.rows
		quotaCache.RUnlock()
		return rows
	}
	quotaCache.RUnlock()

	rows := h.buildAccountQuotas()
	quotaCache.Lock()
	quotaCache.rows = rows
	quotaCache.at = time.Now()
	quotaCache.Unlock()
	return rows
}

// buildAccountQuotas 为每个池账号产出一行配额快照（/v1/quota 与固定号 quota 共用）。
//
// 账号间并发探测、整体受 quotaProbeTimeout 约束：慢/挂住的账号不会拖垮整个响应。
// 冷却中的账号**跳过**探测（上游正在限流它，再打一次只会延长 429）。
func (h *Handler) buildAccountQuotas() []accountQuota {
	accounts := h.cfg.Pool.List()
	out := make([]accountQuota, len(accounts))

	ctx, cancel := context.WithTimeout(context.Background(), quotaProbeTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for i, st := range accounts {
		aq := accountQuota{
			UID:          st.UID,
			Realm:        st.Realm,
			Nickname:     st.Nickname,
			Credits:      st.Credits,
			Cooling:      st.Cooling,
			CoolKind:     st.CoolKind,
			Reason:       st.Reason,
			Disabled:     st.Disabled,
			SuccessCount: st.SuccessCount,
			ErrTotal:     st.ErrTotal,
		}
		if st.Cooling && !st.Until.IsZero() {
			aq.CoolUntil = st.Until.Format(time.RFC3339)
			aq.CoolRemaining = formatRemaining(time.Until(st.Until))
		}
		out[i] = aq

		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			out[i].Error = "account not in pool; no credential available for a quota probe"
			continue
		}
		if st.Cooling {
			out[i].Error = "cooling: quota probe skipped to avoid extending rate limit"
			continue
		}

		wg.Add(1)
		// 每个探测独立成 goroutine；各自写自己的下标，无数据竞争。
		// a 是循环体内声明的变量（每轮独立），闭包捕获安全。
		go func(i int) {
			defer wg.Done()
			pkgs, err := h.cfg.Upstream.ResourcePackages(a)
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			quotas := make(map[string]upstream.ResourcePackage, len(pkgs))
			seen := map[string]int{}
			for _, p := range pkgs {
				name := p.PackageName
				seen[name]++
				if seen[name] > 1 {
					// 同名套餐（如多个「奖励包」）加序号后缀，避免 map 覆盖丢行。
					name = fmt.Sprintf("%s %d", name, seen[name])
				}
				quotas[name] = p
			}
			out[i].Quotas = quotas
		}(i)
	}

	// 等待全部探测完成或整体超时；超时后未完成的账号标记为"未知"。
	// 各 goroutine 因自身 HTTP 超时最终会退出，不会泄漏。
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		for i := range out {
			if out[i].Quotas == nil && out[i].Error == "" {
				out[i].Error = "quota probe timed out"
			}
		}
	}
	return out
}

// formatRemaining 把冷却剩余时长格式化成 "2h 13m 05s" 风格，
// 便于看板直接展示（与 ISO 截止时间并列给出）。
func formatRemaining(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// accountQuota 单个池账号的积分/冷却快照（一条看板行）。
type accountQuota struct {
	UID      string `json:"uid"`
	Realm    string `json:"realm,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Credits  int64  `json:"credits"`
	Cooling  bool   `json:"cooling"`
	CoolKind string `json:"cool_kind,omitempty"`
	// CoolUntil 当前冷却的 ISO 截止时刻；CoolRemaining 是同一时刻的
	// "2h 13m 05s" 预格式化串（看板两列都要，省一次前端计算）。
	CoolUntil     string                              `json:"cool_until,omitempty"`
	CoolRemaining string                              `json:"cool_remaining,omitempty"`
	Reason        string                              `json:"reason,omitempty"`
	Disabled      bool                                `json:"disabled"`
	SuccessCount  int64                               `json:"success_count,omitempty"`
	ErrTotal      int64                               `json:"err_total,omitempty"`
	Quotas        map[string]upstream.ResourcePackage `json:"quotas"`
	// Error 本账号探测失败/被跳过的原因（存在时 Quotas 为空）——
	// 看板据此区分"没有套餐"与"没探到"，不把失败渲染成 0。
	Error string `json:"error,omitempty"`
}
