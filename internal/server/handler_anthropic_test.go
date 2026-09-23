package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// searchUpstreamBody 上游 /agenttool/v1/search 的成功响应（形态取自 2026-09-23 线上实测）。
const searchUpstreamBody = `{"query":"DeepSeek V4","type":"text2text","provider":"0","results":[
 {"title":"模型卡","url":"https://fe-static.deepseek.com/x.pdf","snippet":"发布日期:4 月 27 日","site":"deepseek.com"},
 {"title":"百度百科","url":"https://baike.baidu.com/item/x","snippet":"9月8日...","site":"baike.baidu.com"}
],"total_results":2,"response_time_ms":1200}`

// anthropicSearchReq 构造 DSH 搜索提供方实际发出的请求体（逐字对齐插件 provider：
// 单条 user 消息 + 固定前缀文本；tools/max_tokens 对本端点无意义但保持同形）。
func anthropicSearchReq(text string) *strings.Reader {
	b, _ := json.Marshal(map[string]any{
		"model":      "deepseek-v4-flash",
		"max_tokens": 4096,
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		}},
		"tools": []map[string]any{{"type": "web_search_20250305", "name": "web_search", "max_uses": 5}},
	})
	return strings.NewReader(string(b))
}

// anthropicSearchResponse 消费方（DSH 插件）读取的字段子集。
type anthropicSearchResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Model   string `json:"model"`
	Content []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		Content   []struct {
			Type  string `json:"type"`
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"content"`
		Citations []struct {
			URL       string `json:"url"`
			CitedText string `json:"cited_text"`
		} `json:"citations"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

func decodeAnthropicSearch(t *testing.T, body []byte) anthropicSearchResponse {
	t.Helper()
	var out anthropicSearchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("response is not json: %v body=%s", err, body)
	}
	return out
}

// TestAnthropicSearchOK 正常链路：抠 query → 打上游 → 拼出消费方契约要求的响应形状。
func TestAnthropicSearchOK(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, searchUpstreamBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, anthropicSearchPath,
		anthropicSearchReq("Perform a web search for the query: DeepSeek V4")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeAnthropicSearch(t, rec.Body.Bytes())
	if resp.Type != "message" || resp.Role != "assistant" || resp.StopReason != "end_turn" {
		t.Errorf("envelope wrong: %+v", resp)
	}
	if resp.Model != "deepseek-v4-flash" {
		t.Errorf("model=%q want echo of request model", resp.Model)
	}
	if !strings.HasPrefix(resp.ID, "msg_") {
		t.Errorf("id=%q want msg_ prefix", resp.ID)
	}
	// 契约 1：必须恰好有一个 web_search_tool_result 块（消费方无此块即报错）。
	if len(resp.Content) < 1 || resp.Content[0].Type != "web_search_tool_result" {
		t.Fatalf("first block must be web_search_tool_result, got %+v", resp.Content)
	}
	block := resp.Content[0]
	if block.ToolUseID == "" {
		t.Error("web_search_tool_result.tool_use_id must be non-empty")
	}
	// 契约 2：块内每项 type=="web_search_result" 且 url 是非空字符串（消费方裸取 .length）。
	if len(block.Content) != 2 {
		t.Fatalf("want 2 result items, got %+v", block.Content)
	}
	for i, it := range block.Content {
		if it.Type != "web_search_result" {
			t.Errorf("item[%d].type=%q want web_search_result", i, it.Type)
		}
		if it.URL == "" {
			t.Errorf("item[%d].url must be a non-empty string", i)
		}
		if it.Title == "" {
			t.Errorf("item[%d].title lost", i)
		}
	}
	// 契约 3：snippet 只经同级 text 块的 citations 传递。
	if len(resp.Content) != 2 || resp.Content[1].Type != "text" {
		t.Fatalf("want a trailing text block carrying citations, got %+v", resp.Content)
	}
	if len(resp.Content[1].Citations) != 2 || resp.Content[1].Citations[0].CitedText == "" {
		t.Errorf("citations mismatch: %+v", resp.Content[1].Citations)
	}
}

// TestAnthropicSearchContractNoResults 上游没搜到也必须回**带块**的成功响应：
// 空结果集若省掉 web_search_tool_result 块，消费方会把它当故障报错。
func TestAnthropicSearchContractNoResults(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"query":"x","results":[],"total_results":0}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, anthropicSearchPath,
		anthropicSearchReq("Perform a web search for the query: 查不到的词")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeAnthropicSearch(t, rec.Body.Bytes())
	if len(resp.Content) == 0 || resp.Content[0].Type != "web_search_tool_result" {
		t.Fatalf("empty result set must still carry the result block: %+v", resp.Content)
	}
	if len(resp.Content[0].Content) != 0 {
		t.Errorf("want 0 items, got %+v", resp.Content[0].Content)
	}
}

// TestAnthropicSearchQueryExtraction query 抠取的各种形态。
func TestAnthropicSearchQueryExtraction(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // "" = 期望 400
	}{
		{
			name: "标准前缀",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: golang"}]}]}`,
			want: "golang",
		},
		{
			name: "前缀后带前后空白",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"  Perform a web search for the query:   golang  "}]}]}`,
			want: "golang",
		},
		{
			name: "无前缀兜底整段",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"直接就是查询词"}]}]}`,
			want: "直接就是查询词",
		},
		{
			name: "content 为字符串形态",
			body: `{"messages":[{"role":"user","content":"Perform a web search for the query: plain string"}]}`,
			want: "plain string",
		},
		{
			name: "多块文本按序拼接",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: a"},{"type":"text","text":"b"}]}]}`,
			want: "a\nb",
		},
		{
			name: "取最后一条 user 消息",
			body: `{"messages":[{"role":"user","content":"旧问题"},{"role":"assistant","content":"答案"},{"role":"user","content":"Perform a web search for the query: 新问题"}]}`,
			want: "新问题",
		},
		{
			name: "无文本内容",
			body: `{"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`,
			want: "",
		},
		{name: "messages 为空", body: `{"messages":[]}`, want: ""},
		{name: "畸形 JSON", body: `{`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				hit++
				return 200, searchUpstreamBody, false
			})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, anthropicSearchPath, strings.NewReader(tc.body)))
			if tc.want == "" {
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
				}
				if hit != 0 {
					t.Errorf("must not hit upstream on unparsable request (hits=%d)", hit)
				}
				return
			}
			if rec.Code != http.StatusOK || hit != 1 {
				t.Fatalf("status=%d hits=%d want 200/1 body=%s", rec.Code, hit, rec.Body.String())
			}
		})
	}
}

// TestAnthropicSearchRotatesOnUpstreamFailure 上游失败换号重试：坏号 500 → 好号 200。
func TestAnthropicSearchRotatesOnUpstreamFailure(t *testing.T) {
	var tokens []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		tokens = append(tokens, authz)
		if strings.HasSuffix(authz, "at1") {
			return 500, `{"msg":"boom"}`, false
		}
		return 200, searchUpstreamBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, anthropicSearchPath,
		anthropicSearchReq("Perform a web search for the query: x")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if len(tokens) != 2 {
		t.Fatalf("want 2 upstream attempts (rotate once), got %v", tokens)
	}
	if decodeAnthropicSearch(t, rec.Body.Bytes()).ID == "" {
		t.Error("missing response id")
	}
}

// TestAnthropicSearchDoesNotPunishPool 设计红线：搜索失败不得冷却/禁用账号。
// 次级功能的失败（路径级 404、IP 级 429）若写进池状态，会连带把 chat 也拖下水。
func TestAnthropicSearchDoesNotPunishPool(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests, `{"code":6004,"msg":"rate limit"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, anthropicSearchPath,
		anthropicSearchReq("Perform a web search for the query: x")))
	if rec.Code == http.StatusOK {
		t.Fatalf("want failure status, got 200 body=%s", rec.Body.String())
	}
	total, healthy, cooling, disabled, _ := p.CountsDetailed()
	if total != 2 || healthy != 2 || cooling != 0 || disabled != 0 {
		t.Errorf("search failures must not alter pool state: total=%d healthy=%d cooling=%d disabled=%d",
			total, healthy, cooling, disabled)
	}
	// 错误信封必须带可读 message（消费方读 error.message）。
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Message == "" {
		t.Errorf("error envelope must carry error.message: %s", rec.Body.String())
	}
}

// TestAnthropicSearchNoAccount 池里无 CN 可用号 → 503（消费方据此报"搜索不可用"）。
func TestAnthropicSearchNoAccount(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, searchUpstreamBody, false })
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, anthropicSearchPath,
		anthropicSearchReq("Perform a web search for the query: x")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
}

// TestAnthropicSearchAuth 与既有端点同鉴权口径：带 key 放行，无/错 key 401。
func TestAnthropicSearchAuth(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, searchUpstreamBody, false })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "k1"})

	req := httptest.NewRequest(http.MethodPost, anthropicSearchPath, anthropicSearchReq("Perform a web search for the query: x"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status=%d want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, anthropicSearchPath, anthropicSearchReq("Perform a web search for the query: x"))
	req.Header.Set("Authorization", "Bearer k1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with key: status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	// GET 未注册 → 405（与既有端点同 mux 语义）。
	req = httptest.NewRequest(http.MethodGet, anthropicSearchPath, nil)
	req.Header.Set("Authorization", "Bearer k1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status=%d want 405", rec.Code)
	}
}

// TestAnthropicSearchTimeoutBudget 请求带 context 取消时不挂死（消费方断连即中止）。
func TestAnthropicSearchTimeoutBudget(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, searchUpstreamBody, false })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest(http.MethodPost, anthropicSearchPath,
		anthropicSearchReq("Perform a web search for the query: x"))
	if !strings.Contains(req.URL.Path, "/agenttool/v1/messages") {
		t.Fatalf("path wiring changed: %s", req.URL.Path)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	// 回显模型名（请求未带 model 时的默认值）。
	if got := decodeAnthropicSearch(t, rec.Body.Bytes()).Model; got != anthropicSearchModelDefault {
		t.Errorf("model=%q want %q", got, anthropicSearchModelDefault)
	}
}

// TestAnthropicSearchContentText 直测 content 解析的两个形态。
func TestAnthropicSearchContentText(t *testing.T) {
	if got := contentText(json.RawMessage(`"hello"`)); got != "hello" {
		t.Errorf("string content=%q", got)
	}
	if got := contentText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"tool_use"},{"type":"text","text":"b"}]`)); got != "a\nb" {
		t.Errorf("block content=%q want a\\nb", got)
	}
	if got := contentText(json.RawMessage(`not-json`)); got != "" {
		t.Errorf("garbage content=%q want empty", got)
	}
	if got := contentText(nil); got != "" {
		t.Errorf("nil content=%q want empty", got)
	}
}
