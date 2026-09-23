package metrics

import (
	"os"
	"testing"
	"time"
)

// at 构造本地时区时刻（测试用固定时钟）。
func at(y int, mo time.Month, d, h int) time.Time {
	return time.Date(y, mo, d, h, 0, 0, 0, time.Local)
}

// newClockCollector 构造注入固定时钟的收集器（便于确定性测试分桶）。
func newClockCollector(now *time.Time) *Collector {
	c := New("")
	c.nowFunc = func() time.Time { return *now }
	return c
}

// TestRecordCreatesHourBuckets 每次记录应落入当前小时桶。
func TestRecordCreatesHourBuckets(t *testing.T) {
	now := at(2026, 9, 14, 15)
	c := newClockCollector(&now)

	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 10})
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 20})

	if got := c.SeriesBuckets(); got != 1 {
		t.Fatalf("应有 1 个桶, 得到 %d", got)
	}

	res := c.Range(RangeQuery{Interval: IntervalHour})
	if len(res.Points) != 1 {
		t.Fatalf("应有 1 个数据点, 得到 %d", len(res.Points))
	}
	if res.Points[0].Key != "2026-09-14T15" {
		t.Errorf("桶键 = %q, want 2026-09-14T15", res.Points[0].Key)
	}
	if res.Points[0].Stats.Requests != 2 || res.Points[0].Stats.CompletionTokens != 30 {
		t.Errorf("桶内统计错误: %+v", res.Points[0].Stats)
	}
}

// TestDifferentHoursSeparateBuckets 跨小时应分到不同桶。
func TestDifferentHoursSeparateBuckets(t *testing.T) {
	now := at(2026, 9, 14, 15)
	c := newClockCollector(&now)

	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 10})

	now = at(2026, 9, 14, 16)
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 20})

	res := c.Range(RangeQuery{Interval: IntervalHour})
	if len(res.Points) != 2 {
		t.Fatalf("应有 2 个桶, 得到 %d", len(res.Points))
	}
	// 必须按时间升序。
	if !res.Points[0].Start.Before(res.Points[1].Start) {
		t.Error("数据点应按时间升序")
	}
	if res.Total.CompletionTokens != 30 {
		t.Errorf("区间汇总 = %d, want 30", res.Total.CompletionTokens)
	}
}

// TestAggregateDay 按天聚合应把同一天多个小时桶合并。
func TestAggregateDay(t *testing.T) {
	now := at(2026, 9, 14, 9)
	c := newClockCollector(&now)
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 10})

	now = at(2026, 9, 14, 21)
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 20})

	// 另一天。
	now = at(2026, 9, 15, 3)
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 5})

	res := c.Range(RangeQuery{Interval: IntervalDay})
	if len(res.Points) != 2 {
		t.Fatalf("按天应有 2 个点, 得到 %d", len(res.Points))
	}
	if res.Points[0].Key != "2026-09-14" || res.Points[0].Stats.CompletionTokens != 30 {
		t.Errorf("9/14 聚合错误: key=%q tokens=%d", res.Points[0].Key, res.Points[0].Stats.CompletionTokens)
	}
	if res.Points[1].Key != "2026-09-15" || res.Points[1].Stats.CompletionTokens != 5 {
		t.Errorf("9/15 聚合错误: key=%q tokens=%d", res.Points[1].Key, res.Points[1].Stats.CompletionTokens)
	}
}

// TestAggregateWeek 按周聚合，周一为起点。
//
// 2026-09-14 是周一，2026-09-20 是周日 → 同属一周；
// 2026-09-21（周一）进入下一周。
func TestAggregateWeek(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 10})

	now = at(2026, 9, 20, 23) // 同周周日
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 20})

	now = at(2026, 9, 21, 1) // 下周一
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 40})

	res := c.Range(RangeQuery{Interval: IntervalWeek})
	if len(res.Points) != 2 {
		t.Fatalf("按周应有 2 个点, 得到 %d", len(res.Points))
	}
	if res.Points[0].Stats.CompletionTokens != 30 {
		t.Errorf("第一周应合并 10+20=30, 得到 %d", res.Points[0].Stats.CompletionTokens)
	}
	if res.Points[1].Stats.CompletionTokens != 40 {
		t.Errorf("第二周应为 40, 得到 %d", res.Points[1].Stats.CompletionTokens)
	}
	// 周的起点必须是周一 0 点。
	if res.Points[0].Start.Weekday() != time.Monday {
		t.Errorf("周起点应是周一, 得到 %v", res.Points[0].Start.Weekday())
	}
}

// TestRangeFiltersByTime 时间区间过滤（含 From、不含 To）。
func TestRangeFiltersByTime(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 1})

	now = at(2026, 9, 15, 10)
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 2})

	now = at(2026, 9, 16, 10)
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 4})

	// 只取 9/15 起（含）到 9/16 之前。
	res := c.Range(RangeQuery{
		From:     at(2026, 9, 15, 0),
		To:       at(2026, 9, 16, 0),
		Interval: IntervalDay,
	})
	if len(res.Points) != 1 {
		t.Fatalf("应只命中 1 个桶, 得到 %d", len(res.Points))
	}
	if res.Points[0].Stats.CompletionTokens != 2 {
		t.Errorf("命中桶 token = %d, want 2", res.Points[0].Stats.CompletionTokens)
	}
}

// TestRangeFiltersByModel 按模型过滤。
func TestRangeFiltersByModel(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	c.Record(Delta{Model: "glm-5.1", OK: true, HasUsage: true, CompletionTokens: 10})
	c.Record(Delta{Model: "deepseek", OK: true, HasUsage: true, CompletionTokens: 20})

	res := c.Range(RangeQuery{Interval: IntervalHour, Model: "glm-5.1"})
	if len(res.Points) != 1 {
		t.Fatalf("应有 1 个点, 得到 %d", len(res.Points))
	}
	if res.Points[0].Stats.CompletionTokens != 10 {
		t.Errorf("过滤后 token = %d, want 10", res.Points[0].Stats.CompletionTokens)
	}
	// Models 应只含被过滤的模型。
	if len(res.Models) != 1 || res.Models[0] != "glm-5.1" {
		t.Errorf("Models = %v, want [glm-5.1]", res.Models)
	}
}

// TestRangeIncludesModelsList 不筛模型时，应列出区间内出现过的所有模型。
func TestRangeIncludesModelsList(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	c.Record(Delta{Model: "b-model", OK: true})
	c.Record(Delta{Model: "a-model", OK: true})

	res := c.Range(RangeQuery{Interval: IntervalHour})
	if len(res.Models) != 2 || res.Models[0] != "a-model" || res.Models[1] != "b-model" {
		t.Errorf("Models 应升序 = [a-model b-model], 得到 %v", res.Models)
	}
}

// TestRangeEmpty 无数据时返回空点集，不 panic。
func TestRangeEmpty(t *testing.T) {
	c := New("")
	res := c.Range(RangeQuery{Interval: IntervalDay})
	if len(res.Points) != 0 {
		t.Errorf("无数据应返回空集, 得到 %d 点", len(res.Points))
	}
	if res.Total.Requests != 0 {
		t.Errorf("汇总应为 0, 得到 %d", res.Total.Requests)
	}
}

// TestSeriesAndCumulativeConsistent 时间序列之和必须等于累计统计
// （两者用同一 accumulate 口径，若不一致说明有 bug）。
func TestSeriesAndCumulativeConsistent(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	for i := 0; i < 5; i++ {
		c.Record(Delta{Model: "m", OK: true, HasUsage: true,
			PromptTokens: 10, CompletionTokens: 20, CacheHitTokens: 5})
	}
	now = at(2026, 9, 15, 10)
	for i := 0; i < 3; i++ {
		c.Record(Delta{Model: "m", OK: false, HasUsage: true, CompletionTokens: 7})
	}

	cum := c.Snapshot().Models["m"]
	res := c.Range(RangeQuery{Interval: IntervalDay})

	if int64(res.Total.Requests) != cum.Requests {
		t.Errorf("序列请求数 %d != 累计 %d", res.Total.Requests, cum.Requests)
	}
	if res.Total.CompletionTokens != cum.CompletionTokens {
		t.Errorf("序列输出 token %d != 累计 %d", res.Total.CompletionTokens, cum.CompletionTokens)
	}
	if res.Total.Failed != cum.Failed {
		t.Errorf("序列失败数 %d != 累计 %d", res.Total.Failed, cum.Failed)
	}
}

// TestRetentionPrunesOldBuckets 超出保留期的桶应被清理。
func TestRetentionPrunesOldBuckets(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	c.SetRetention(24 * time.Hour)

	c.Record(Delta{Model: "m", OK: true}) // 9/14 10:00

	// 跳到 40 天后（远超 24h 保留期）。
	now = at(2026, 10, 24, 10)
	c.Record(Delta{Model: "m", OK: true})

	if got := c.SeriesBuckets(); got != 1 {
		t.Errorf("过期桶应被清理，期望 1 个桶，得到 %d", got)
	}
	// 累计统计不受清理影响（那是总量口径）。
	if cum := c.Snapshot().Models["m"]; cum.Requests != 2 {
		t.Errorf("累计统计不应被保留期影响, 得到 %d 请求", cum.Requests)
	}
}

// TestRetentionKeepsRecentBuckets 保留期内的桶必须保留。
func TestRetentionKeepsRecentBuckets(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	c.SetRetention(48 * time.Hour)

	c.Record(Delta{Model: "m", OK: true}) // 9/14 10:00
	now = at(2026, 9, 15, 10)             // +24h，仍在保留期内
	c.Record(Delta{Model: "m", OK: true})

	if got := c.SeriesBuckets(); got != 2 {
		t.Errorf("保留期内应有 2 个桶, 得到 %d", got)
	}
}

// TestSeriesPersisted 时间序列应随累计统计一起落盘并在重启后恢复。
func TestSeriesPersisted(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/metrics.json"

	now := at(2026, 9, 14, 10)
	c := New(fp)
	c.nowFunc = func() time.Time { return now }
	c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 42})
	c.Flush()

	c2 := New(fp)
	res := c2.Range(RangeQuery{Interval: IntervalHour})
	if len(res.Points) != 1 {
		t.Fatalf("重启后应恢复 1 个小时桶, 得到 %d", len(res.Points))
	}
	if res.Points[0].Stats.CompletionTokens != 42 {
		t.Errorf("重启后桶内 token = %d, want 42", res.Points[0].Stats.CompletionTokens)
	}
}

// TestLoadOldFormatWithoutSeries 向后兼容：旧 metrics.json 没有 series 字段时，
// 累计统计照常恢复，时间序列为空（不 panic、不报错）。
func TestLoadOldFormatWithoutSeries(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/metrics.json"
	old := `{"since":"2026-09-14T10:00:00+08:00","models":{"m":{"model":"m","requests":7,"completion_tokens":70}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	c := New(fp)
	if cum := c.Snapshot().Models["m"]; cum == nil || cum.Requests != 7 {
		t.Errorf("旧格式应恢复累计统计, 得到 %+v", cum)
	}
	if got := c.SeriesBuckets(); got != 0 {
		t.Errorf("旧格式无 series，应为 0 桶, 得到 %d", got)
	}
	// 之后记录应正常建立新桶。
	c.Record(Delta{Model: "m", OK: true})
	if got := c.SeriesBuckets(); got != 1 {
		t.Errorf("记录后应有 1 桶, 得到 %d", got)
	}
}

// TestResetClearsSeries 重置应同时清空时间序列。
func TestResetClearsSeries(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	c.Record(Delta{Model: "m", OK: true})
	if c.SeriesBuckets() == 0 {
		t.Fatal("前置条件失败")
	}
	c.Reset()
	if got := c.SeriesBuckets(); got != 0 {
		t.Errorf("重置后应为 0 桶, 得到 %d", got)
	}
}

// TestNormalizeInterval 粒度参数规范化。
func TestNormalizeInterval(t *testing.T) {
	cases := map[string]Interval{
		"":       IntervalHour,
		"hour":   IntervalHour,
		"day":    IntervalDay,
		"daily":  IntervalDay,
		"d":      IntervalDay,
		"week":   IntervalWeek,
		"weekly": IntervalWeek,
		"W":      IntervalWeek,
		"bogus":  IntervalHour,
	}
	for in, want := range cases {
		if got := NormalizeInterval(in); got != want {
			t.Errorf("NormalizeInterval(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestEarliestBucket 返回最早桶起点。
func TestEarliestBucket(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	if !c.EarliestBucket().IsZero() {
		t.Error("无数据时应返回零值")
	}
	c.Record(Delta{Model: "m", OK: true})
	now = at(2026, 9, 15, 3)
	c.Record(Delta{Model: "m", OK: true})

	if got := c.EarliestBucket(); !got.Equal(at(2026, 9, 14, 10)) {
		t.Errorf("最早桶 = %v, want %v", got, at(2026, 9, 14, 10))
	}
}

// TestConcurrentRangeAndRecord 记录与查询并发不应竞态（-race 下有完整意义）。
func TestConcurrentRangeAndRecord(t *testing.T) {
	now := at(2026, 9, 14, 10)
	c := newClockCollector(&now)
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			c.Record(Delta{Model: "m", OK: true, HasUsage: true, CompletionTokens: 1})
		}
	}()
	for i := 0; i < 50; i++ {
		_ = c.Range(RangeQuery{Interval: IntervalDay})
		_ = c.SeriesBuckets()
	}
	<-done

	res := c.Range(RangeQuery{Interval: IntervalHour})
	if res.Total.Requests != 100 {
		t.Errorf("并发后请求数 = %d, want 100", res.Total.Requests)
	}
}
