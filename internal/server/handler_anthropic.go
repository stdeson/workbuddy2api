// handler_anthropic.go DSH（DeepSeek Harness）web 搜索后端适配端点。
//
// 为什么是这个形状（任务书 dsh-web-search）：
//   - DSH 的搜索提供方 @deepseek-ai/dsh-web-search-deepseek 只会说 Anthropic Messages：
//     它 POST {baseURL}/messages，body 里放一条用户文本
//     "Perform a web search for the query: <query>" + 一个原生 web_search 服务器工具，
//     然后**只取**响应 content[] 里的 web_search_tool_result 块——提供方文本一律丢弃
//     （插件源码：mapAnthropicResponse + "DeepSeek 的提供方文本不作为答案受到信任"）。
//   - 因此本端点不必跑模型、也不必实现 Anthropic 协议：抠出 query → 调上游
//     /agenttool/v1/search → 把结果拼成那个块即可。零 token、零积分、约 2 秒。
//   - 路径故意不叫 /anthropic：这里不是通用 Anthropic API，只是搜索适配器。
//
// 消费方解析契约（违反即报错，DSH 不降级）：
//  1. content 必须含 ≥1 个 type=="web_search_tool_result" 块，否则 WEB_PROVIDER_ERROR；
//  2. 块内每项须 type=="web_search_result" 且 **url 是非空字符串**——插件里是裸取
//     item.url.length（无 ?. 保护），字段缺失直接 TypeError 被当成"响应不可解析"；
//  3. snippet 只能经同级 text 块的 citations[].{url,cited_text} 传递（同名 URL 取首条）；
//  4. 错误响应读 error / error.message / message（见 writeAnthropicError）。
//
// 设计决策：搜索不惩罚账号。本端点是次级只读功能，其失败（尤其是路径级 404、IP 级
// 429）不得反向冷却/禁用/喂连败到账号池——那会连带影响 chat 可用性。故除 token 预刷新
// （正向维护，与 chat 同款）外，本路径对 pool 状态零写入，失败只在本请求内换号。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

const (
	// anthropicSearchPath DSH 搜索提供方拼出的路径：{baseURL}/messages。把 baseURL 配成
	// https://<网关>/agenttool/v1，故此处是 /agenttool/v1/messages。
	anthropicSearchPath = "/agenttool/v1/messages"
	// anthropicSearchBodyLimit 入站请求体上限：DSH 的 body 只有一段短文本 + 工具声明，
	// 64KB 绰绰有余；设限防超大 body 打满内存。
	anthropicSearchBodyLimit = 64 << 10
	// anthropicSearchTimeout 单次搜索的出站总时长上限（含换号重试；实测单次约 2.1s）。
	anthropicSearchTimeout = 30 * time.Second
	// anthropicSearchRealm 搜索只在 CN 域发起：/agenttool/v1/search 实测在
	// copilot.tencent.com；global（workbuddy.ai）是否同构未验证，不把 global 号拉来试错。
	anthropicSearchRealm = "cn"
	// searchQueryPrefix DSH 固定的用户文本前缀（插件 provider 内硬编码，逐字匹配）。
	searchQueryPrefix = "Perform a web search for the query: "
	// anthropicSearchModelDefault 请求未带 model 时回显的模型名（消费方不使用该字段）。
	anthropicSearchModelDefault = "deepseek-v4-flash"
)

// anthropicSearchRequest DSH 发来的 Messages 请求中本端点用到的字段。
// max_tokens / tools / anthropic-version 对搜索适配器无意义，忽略。
type anthropicSearchRequest struct {
	Model    string             `json:"model"`
	Messages []anthropicMessage `json:"messages"`
}

// anthropicMessage 一条消息：content 可能是字符串，也可能是块数组，故延迟解析。
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicContentBlock content 数组形态里的单个块（只关心 text 块）。
type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// anthropicMessagesPost DSH 搜索请求入口（POST /agenttool/v1/messages）。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, anthropicSearchBodyLimit))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req anthropicSearchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	query := extractSearchQuery(req.Messages)
	if query == "" {
		writeAnthropicError(w, http.StatusBadRequest,
			`no "`+searchQueryPrefix+`…" text part found in messages`)
		return
	}
	model := req.Model
	if model == "" {
		model = anthropicSearchModelDefault
	}
	// 超时覆盖整个换号循环（客户端断连经 r.Context() 立即中断在途出站）。
	ctx, cancel := context.WithTimeout(r.Context(), anthropicSearchTimeout)
	defer cancel()

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcludingForRealm(tried, "", anthropicSearchRealm)
		if acct == nil {
			break // 无 CN 可用号（或全部试过）
		}
		tried[acct.UID] = true
		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue
		}
		// token 临近过期 → 先 refresh（与 chat 同款；RefreshToken 自带并发写回守卫，
		// 与其他路径并发刷新安全）。失败只换号，不罚号也不禁用——账号健康由 chat 裁决。
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if rerr := h.cfg.Upstream.RefreshToken(acct); rerr != nil {
				h.cfg.Pool.Release(acct.UID)
				lastErr = rerr
				log.Printf("WARN: [server] search refresh acct=%s: %v", logfmt.Label(acct.UID, acct.Nickname), rerr)
				continue
			}
			acct.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
			if serr := acct.SaveAtomic(); serr != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露（与 chat 同口径）。
				log.Printf("ERR: [server] search refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), serr)
			}
		}
		results, serr := h.cfg.Upstream.Search(ctx, acct, query, upstream.DefaultSearchMaxResults)
		h.cfg.Pool.Release(acct.UID)
		if serr == nil {
			writeAnthropicSearchOK(w, model, results)
			return
		}
		lastErr = serr
		log.Printf("WARN: [server] search acct=%s: %v", logfmt.Label(acct.UID, acct.Nickname), serr)
		if !rotateBackoff(i, r.Context()) {
			break // 客户端已断连：换号重试无意义
		}
	}
	if lastErr == nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "no healthy account available in pool")
		return
	}
	var uerr *upstream.Error
	if errors.As(lastErr, &uerr) {
		writeAnthropicError(w, http.StatusBadGateway, uerr.Error())
		return
	}
	writeAnthropicError(w, http.StatusBadGateway, lastErr.Error())
}

// extractSearchQuery 从 messages 里抠出搜索词：取**最后一条** user 消息的文本，剥掉
// DSH 的固定前缀。多轮扩展下取最后一条仍正确；剥不掉前缀时整段当 query（兜底：
// 前缀属消费方实现细节，未来若变形也不至于让搜索彻底失效）。
func extractSearchQuery(msgs []anthropicMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "" && msgs[i].Role != "user" {
			continue
		}
		text := contentText(msgs[i].Content)
		if text == "" {
			continue
		}
		trimmed := strings.TrimSpace(text)
		if q := strings.TrimSpace(strings.TrimPrefix(trimmed, searchQueryPrefix)); q != "" {
			return q
		}
	}
	return ""
}

// contentText 取 content 的纯文本：兼容字符串形态与 [{type:"text",text:"…"}] 数组形态
// （Anthropic 两种都合法），多块按序换行拼接；非 text 块忽略。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(b.Text)
	}
	return sb.String()
}

// writeAnthropicSearchOK 按消费方契约组装 Messages 响应。
//
// 刻意不含任何"模型回答"：消费方明确不信任提供方文本，只取结构化结果块；因此 text 块
// 只用于承载 citations（snippet 的唯一通路），且仅在有 snippet 时才发。
func writeAnthropicSearchOK(w http.ResponseWriter, model string, results []upstream.SearchResult) {
	items := make([]map[string]any, 0, len(results))
	citations := make([]map[string]any, 0, len(results))
	for _, r := range results {
		// url 必为字符串（upstream.Search 已挡掉空 URL 条目）——消费方裸取 .length。
		item := map[string]any{"type": "web_search_result", "url": r.URL}
		if r.Title != "" {
			item["title"] = r.Title
		}
		// page_age 留空：上游 results[] 不含发布日期字段（只有 title/url/snippet/site/
		// score/content/highlights），编造日期比缺失更糟。
		items = append(items, item)
		if r.Snippet != "" {
			citations = append(citations, map[string]any{
				"type":       "web_search_result_location",
				"url":        r.URL,
				"cited_text": r.Snippet,
			})
		}
	}
	blocks := []map[string]any{{
		"type":        "web_search_tool_result",
		"tool_use_id": "srvtoolu_" + randomHex(12),
		"content":     items,
	}}
	if len(citations) > 0 {
		blocks = append(blocks, map[string]any{
			"type":      "text",
			"text":      "",
			"citations": citations,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          "msg_" + randomHex(12),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     blocks,
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
	})
}

// writeAnthropicError Anthropic 风格错误信封。消费方按 error → error.message → message
// 顺序取消息，故 message 放最里层；HTTP 状态码同时携带语义（非 2xx 即失败）。
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": msg,
		},
	})
}

// randomHex 生成 n 字节的随机十六进制串（仅用于响应 id，非安全用途但无需弱化随机源）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
