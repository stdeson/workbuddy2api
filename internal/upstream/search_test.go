package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// searchOKBody 一份接近上游真实形态的搜索响应（2026-09-23 线上探测原文节选）。
const searchOKBody = `{"query":"DeepSeek V4 发布时间","type":"text2text","provider":"0",
"results":[
 {"title":"DeepSeek V4 - 模型卡","url":"https://fe-static.deepseek.com/x.pdf","snippet":"发布日期:4 月 27 日","site":"deepseek.com"},
 {"title":"百度百科","url":"https://baike.baidu.com/item/x","snippet":"9月8日...","site":"baike.baidu.com"}
],"total_results":2,"response_time_ms":1200}`

func TestSearchOK(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotUID, gotCBReq string
	var gotBody map[string]any
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuth, gotUID = r.Header.Get("Authorization"), r.Header.Get("X-User-Id")
		gotCBReq = r.Header.Get("X-CodeBuddy-Request")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("request body not json: %v (%s)", err, raw)
		}
		return jsonResp(200, searchOKBody), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at1"}
	res, err := c.Search(context.Background(), a, "DeepSeek V4 发布时间", 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// 出站形态：路径拼在 chatBase 上、POST、带账号身份三头。
	if gotPath != "/agenttool/v1/search" {
		t.Errorf("path=%q want /agenttool/v1/search", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method=%q want POST", gotMethod)
	}
	if gotAuth != "Bearer at1" {
		t.Errorf("authorization=%q want Bearer at1", gotAuth)
	}
	if gotUID != "u1" {
		t.Errorf("x-user-id=%q want u1", gotUID)
	}
	if gotCBReq != "1" {
		t.Errorf("x-codebuddy-request=%q want 1", gotCBReq)
	}
	// 请求体契约：官方扩展硬编码 type=text2text；maxResults<=0 落默认。
	if gotBody["query"] != "DeepSeek V4 发布时间" || gotBody["type"] != searchType {
		t.Errorf("body query/type wrong: %v", gotBody)
	}
	if gotBody["language"] != "zh" {
		t.Errorf("language=%v want zh (query 含 CJK)", gotBody["language"])
	}
	if n, _ := gotBody["max_results"].(float64); int(n) != DefaultSearchMaxResults {
		t.Errorf("max_results=%v want %d", gotBody["max_results"], DefaultSearchMaxResults)
	}
	// 响应投影。
	if len(res) != 2 {
		t.Fatalf("results=%d want 2: %+v", len(res), res)
	}
	if res[0].URL != "https://fe-static.deepseek.com/x.pdf" || res[0].Title != "DeepSeek V4 - 模型卡" ||
		res[0].Snippet != "发布日期:4 月 27 日" || res[0].Site != "deepseek.com" {
		t.Errorf("first result mismatch: %+v", res[0])
	}
}

// TestSearchSkipsEmptyURL 空/缺失 URL 的条目必须被挡在网关侧：消费方解析器裸取
// item.url.length，缺字段会直接 TypeError（不是"少一条结果"，是整次搜索报错）。
func TestSearchSkipsEmptyURL(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"results":[{"title":"ok","url":"https://a.example"},
			{"title":"no-url"},{"title":"blank","url":"   "},{"title":"second","url":"https://b.example"}]}`), nil
	})
	res, err := c.Search(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "q", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 2 || res[0].URL != "https://a.example" || res[1].URL != "https://b.example" {
		t.Fatalf("want only non-empty URLs, got %+v", res)
	}
}

// TestSearchEmptyQueryNoNetwork 空 query 不发请求（消费方抠不出 query 时不该白打上游）。
func TestSearchEmptyQueryNoNetwork(t *testing.T) {
	called := false
	c := testClient(func(*http.Request) (*http.Response, error) {
		called = true
		return jsonResp(200, `{}`), nil
	})
	for _, q := range []string{"", "   ", "\n"} {
		res, err := c.Search(context.Background(), &auth.Auth{UID: "u1"}, q, 5)
		if err != nil || res != nil {
			t.Fatalf("q=%q want (nil,nil) got (%v,%v)", q, res, err)
		}
	}
	if called {
		t.Error("empty query must not hit the network")
	}
}

// TestSearchNoResultsIsNotAnError 「没搜到」是正常结果，不是故障（消费方会拿到 0 条来源）。
func TestSearchNoResultsIsNotAnError(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"query":"x","results":[],"total_results":0}`), nil
	})
	res, err := c.Search(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "x", 5)
	if err != nil {
		t.Fatalf("empty result set must not error: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("want 0 results, got %+v", res)
	}
}

// TestSearchNon200Classified 非 2xx 走 *Error 信封：Kind 由 Classify 判定，
// Retry-After 头解析进 RetryAfter（与 chat 同口径，调用方据此决定冷却）。
func TestSearchNon200Classified(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		resp := jsonResp(http.StatusTooManyRequests, `{"code":6004,"msg":"rate limit"}`)
		resp.Header.Set("Retry-After", "2")
		return resp, nil
	})
	_, err := c.Search(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "q", 5)
	if err == nil {
		t.Fatal("want error on 429")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("want *upstream.Error, got %T: %v", err, err)
	}
	if ue.Kind != ErrSoftRate || ue.Status != http.StatusTooManyRequests {
		t.Errorf("kind/status=%v/%d want %v/429", ue.Kind, ue.Status, ErrSoftRate)
	}
	if ue.RetryAfter != 2*time.Second {
		t.Errorf("retryAfter=%v want 2s", ue.RetryAfter)
	}
}

// TestSearchParseFailureIsPlainError 200 但 body 不是 JSON → 普通错误（非 *Error）：
// 调用方按"未知失败"只换号，不当作账号级信号。
func TestSearchParseFailureIsPlainError(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `<html>gateway page</html>`), nil
	})
	_, err := c.Search(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "q", 5)
	if err == nil {
		t.Fatal("want error on unparsable body")
	}
	if _, ok := err.(*Error); ok {
		t.Errorf("unparsable body must not be classified as *Error: %v", err)
	}
}

// TestSearchTransportErrorIsPlainError 传输层失败同理。
func TestSearchTransportErrorIsPlainError(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	_, err := c.Search(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "q", 5)
	if err == nil || !strings.Contains(err.Error(), "search") {
		t.Fatalf("want transport error, got %v", err)
	}
}

// TestSearchLanguage 语种推断：含 CJK → zh，纯 ASCII/拉丁 → en。
func TestSearchLanguage(t *testing.T) {
	cases := map[string]string{
		"DeepSeek V4 发布时间":    "zh",
		"最新消息":                "zh",
		"DeepSeek V4 release": "en",
		"café résumé":         "en", // 拉丁扩展（U+00E9 < U+2E80）不算 CJK
		"":                    "en",
	}
	for q, want := range cases {
		if got := searchLanguage(q); got != want {
			t.Errorf("searchLanguage(%q)=%q want %q", q, got, want)
		}
	}
}
