// search.go 上游搜索接口（/agenttool/v1/search）客户端。
//
// 背景（任务书 dsh-web-search）：CodeBuddy 的 web_search 是**客户端工具**——模型只回
// tool_calls，真正的检索由 IDE 扩展自己发 HTTP 执行（extensions/genie/out/extension/
// index.js 的 fetchSearchResults）。该请求与 chat 同域（copilot.tencent.com）、同凭据
// （账号 accessToken），所以网关无需新增上游或密钥，复用现有号池即可。
//
// 线上实测（2026-09-23，5 个号连打 6 次）：HTTP 200、单次约 2.1s、返回结构化结果、
// **不计积分**（调用前后 get-user-resource 的 remain 无变化）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

const (
	// searchPath 搜索接口路径（按 chatBase 切 CN/global base）。
	searchPath = "/agenttool/v1/search"
	// searchType 上游搜索类型：text2text 是官方扩展硬编码的唯一取值。
	searchType = "text2text"
	// DefaultSearchMaxResults 默认请求结果条数（消费方 DSH 还会按自己的上限二次截断）。
	DefaultSearchMaxResults = 10
	// searchBodyLimit 响应体读取上限：实测单次 <20KB，1MB 足够且防上游异常巨体。
	searchBodyLimit = 1 << 20
)

// SearchResult 单条搜索结果（上游 results[] 元素在网关侧的投影）。
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
	Site    string
}

// Search 调一次上游搜索。query 为空白或上游无结果都返回空切片——"没搜到"是正常结果，
// 不是故障。非 2xx 返回 *Error（Kind 走 Classify、Retry-After 头解析，与 chat 同口径）；
// 传输层/解析失败返回普通 error。
//
// 本函数只做一次出站，**不重试、不换号**——轮转与错误处置由 server 层负责。
// 结果条数 maxResults <= 0 时取 DefaultSearchMaxResults。
// language 由 query 语种推断（含 CJK → zh，否则 en），上游按该字段决定检索语料偏好。
func (c *Client) Search(ctx context.Context, a *auth.Auth, query string, maxResults int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if maxResults <= 0 {
		maxResults = DefaultSearchMaxResults
	}
	payload, err := json.Marshal(map[string]any{
		"query":       query,
		"type":        searchType,
		"max_results": maxResults,
		"language":    searchLanguage(query),
	})
	if err != nil {
		return nil, fmt.Errorf("search marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+searchPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	// 复用共享头（Content-Type/Accept/Origin/Referer/UA/X-CodeBuddy-Request/账号稳定头），
	// 再补 chat 同款的账号身份头。实测这组头即可通过（线上探测只用了 Bearer + X-User-Id）。
	c.CommonHeaders(req, a)
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, searchBodyLimit))
	if err != nil {
		// 读失败 → 传输层错误（非 *Error）：调用方只换号不罚号，与既有口径一致。
		return nil, fmt.Errorf("search read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		uerr := &Error{
			Kind:   Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode,
			Msg:    string(raw),
		}
		if d, ok := ParseRetryAfter(resp.Header); ok {
			uerr.RetryAfter = d
		}
		return nil, uerr
	}
	var env struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
			Site    string `json:"site"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("search parse: %w (body: %s)", err, truncate(string(raw), 120))
	}
	out := make([]SearchResult, 0, len(env.Results))
	for _, r := range env.Results {
		// 空 URL 的结果对消费方无用，且其解析器裸取 item.url.length（缺字段直接
		// TypeError），必须挡在网关侧——网关只透出可安全映射的条目。
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		out = append(out, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet, Site: r.Site})
	}
	return out, nil
}

// searchLanguage 按 query 是否含 CJK 推断检索语言：含 → zh，否则 en。
// 上游把 language 当普通检索参数透传；中文用户也会搜英文词条，故按 query 判而非按账号域。
func searchLanguage(query string) string {
	for _, r := range query {
		if r >= 0x2E80 { // CJK 部首起始码位：覆盖汉字/假名/韩文，ASCII 与拉丁扩展不受影响
			return "zh"
		}
	}
	return "en"
}
