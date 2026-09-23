package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/metrics"
)

// statsRangeResp /v1/stats 时间维度响应的断言形状（字段名与社区面板约定一致）。
type statsRangeResp struct {
	SeriesBuckets *int `json:"series_buckets"`
	Range         *struct {
		Interval string `json:"interval"`
		Points   []struct {
			Key   string `json:"key"`
			Stats struct {
				Requests  int64 `json:"requests"`
				Success   int64 `json:"success"`
				Streaming int64 `json:"streaming"`
			} `json:"stats"`
			Derived struct {
				Requests  int64   `json:"requests"`
				AvgTTFBMS float64 `json:"avg_ttfb_ms"`
			} `json:"derived"`
		} `json:"points"`
		Total struct {
			Requests int64 `json:"requests"`
		} `json:"total"`
		Models []string `json:"models"`
	} `json:"range"`
}

// getStats 发一次 GET /v1/stats（带可选 query）并解析时间维度字段。
func getStats(t *testing.T, h *Handler, query string) statsRangeResp {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/stats"+query, nil))
	if rec.Code != 200 {
		t.Fatalf("GET /v1/stats%s code=%d body=%s", query, rec.Code, rec.Body)
	}
	var out statsRangeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode stats: %v body=%s", err, rec.Body)
	}
	return out
}

// TestStatsTimeSeriesRange 面板「时间趋势」的完整链路：一次 chat 请求 → 收集器落桶 →
// /v1/stats?range=today&interval=day 返回 range（含数据点与区间汇总）+ series_buckets。
//
// 这是本次回移的核心验收：社区面板在 range 缺失时永远显示"正在加载趋势数据…"。
func TestStatsTimeSeriesRange(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 内存收集器（stateFile=""）：测试不落盘，避免污染仓库工作目录。
	collector := metrics.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Metrics: collector})

	body := `{"model":"glm-5.2","messages":[],"stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body))))
	if rec.Code != 200 {
		t.Fatalf("chat code=%d body=%s", rec.Code, rec.Body)
	}

	got := getStats(t, h, "?range=today&interval=day")
	if got.SeriesBuckets == nil {
		t.Fatal("series_buckets 缺失：面板会显示「正在加载趋势数据…」占位")
	}
	if *got.SeriesBuckets < 1 {
		t.Errorf("series_buckets=%d want >=1（刚写过一次请求）", *got.SeriesBuckets)
	}
	if got.Range == nil {
		t.Fatal("range 缺失（带 range/interval 参数时必须返回区间序列）")
	}
	if got.Range.Interval != "day" {
		t.Errorf("interval=%q want day", got.Range.Interval)
	}
	if len(got.Range.Points) != 1 {
		t.Fatalf("points=%d want 1（今天的同一小时桶）", len(got.Range.Points))
	}
	pt := got.Range.Points[0]
	if pt.Stats.Requests != 1 || pt.Stats.Success != 1 || pt.Stats.Streaming != 1 {
		t.Errorf("点统计错: %+v", pt.Stats)
	}
	if pt.Derived.Requests != 1 {
		t.Errorf("派生请求数=%d want 1", pt.Derived.Requests)
	}
	if got.Range.Total.Requests != 1 {
		t.Errorf("区间汇总请求数=%d want 1", got.Range.Total.Requests)
	}
	if len(got.Range.Models) != 1 || got.Range.Models[0] != "glm-5.2" {
		t.Errorf("区间模型列表=%v want [glm-5.2]", got.Range.Models)
	}

	// 粒度切到小时：同一小时桶仍是 1 个点，但 key 形状不同（用于面板粒度切换）。
	hour := getStats(t, h, "?range=today&interval=hour")
	if hour.Range == nil || len(hour.Range.Points) != 1 {
		t.Fatalf("hour 粒度 points=%v", hour.Range)
	}
	if hour.Range.Interval != "hour" {
		t.Errorf("hour interval=%q", hour.Range.Interval)
	}

	// 模型筛选：命中一个、错过一个。
	hit := getStats(t, h, "?range=today&interval=day&model=glm-5.2")
	if hit.Range == nil || hit.Range.Total.Requests != 1 {
		t.Errorf("按模型命中区间请求数=%v want 1", hit.Range)
	}
	miss := getStats(t, h, "?range=today&interval=day&model=no-such-model")
	if miss.Range == nil || miss.Range.Total.Requests != 0 {
		t.Errorf("按模型过滤应无请求: %+v", miss.Range)
	}
}

// TestStatsWithoutTimeParamsKeepsLegacyShape：不带时间参数时不返回 range（旧客户端/旧面板
// 行为逐字不变），但 series_buckets 仍在（面板据此提示可回溯时长）。
func TestStatsWithoutTimeParamsKeepsLegacyShape(t *testing.T) {
	collector := metrics.New("")
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "{}", false
	}), Metrics: collector})

	got := getStats(t, h, "")
	if got.Range != nil {
		t.Errorf("无时间参数不应返回 range: %+v", got.Range)
	}
	if got.SeriesBuckets == nil {
		t.Error("series_buckets 应恒存在（装有收集器时）")
	}
}

// TestStatsWithoutCollectorOmitsSeries：metrics_enabled=false（未装收集器）时，
// 响应里没有 series_buckets/range —— 面板显示"未启用"占位，其余卡片零回归。
// 这是设计好的向后兼容形态，不是错误。
func TestStatsWithoutCollectorOmitsSeries(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "{}", false
	})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/stats?range=today&interval=day", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["series_buckets"]; ok {
		t.Error("未装收集器时不应出现 series_buckets")
	}
	if _, ok := raw["range"]; ok {
		t.Error("未装收集器时不应出现 range")
	}
	if _, ok := raw["models"]; !ok {
		t.Error("既有字段 models 必须保留（面板其他卡片依赖）")
	}
}

// TestStatsResetClearsSeries：/v1/stats/reset 必须把时间序列一起清掉，
// 否则面板"清空看增量"后趋势里仍是旧数据，两本账对不上。
func TestStatsResetClearsSeries(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	collector := metrics.New("")
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, Metrics: collector})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"glm-5.2","messages":[],"stream":true}`)))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := getStats(t, h, "?range=today&interval=day"); got.Range == nil || got.Range.Total.Requests != 1 {
		t.Fatalf("前置条件不成立（应有 1 个请求）: %+v", got.Range)
	}

	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest("POST", "/v1/stats/reset", nil))
	if res.Code != 200 {
		t.Fatalf("reset code=%d", res.Code)
	}
	after := getStats(t, h, "?range=today&interval=day")
	if after.Range == nil {
		t.Fatal("reset 后仍应返回 range")
	}
	if after.Range.Total.Requests != 0 {
		t.Errorf("reset 后区间请求数=%d want 0", after.Range.Total.Requests)
	}
	if after.SeriesBuckets == nil || *after.SeriesBuckets != 0 {
		t.Errorf("reset 后 series_buckets=%v want 0", after.SeriesBuckets)
	}
}
