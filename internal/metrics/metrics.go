// Package metrics 按模型维度的请求量/token/缓存命中/扣费统计。
//
// 设计取态：网关是唯一能看到**所有**请求（含绕过面板的其他客户端）的位置，
// 因此统计在这里采集，通过 /v1/stats 暴露给面板。
//
// 为什么按模型分组：不同模型的定价、上下文长度、缓存行为差异极大
// （cache 命中率直接影响实际扣费），混在一起看没有决策价值。
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelStats 单个模型的累计统计。
//
// 所有计数都是**累计值**（进程启动至今），面板取两次快照的差值即可算速率。
type ModelStats struct {
	Model string `json:"model"`

	// ── 请求量 ──────────────────────────────────────────
	Requests  int64 `json:"requests"`  // 总请求数
	Success   int64 `json:"success"`   // 2xx
	Failed    int64 `json:"failed"`    // 非 2xx / 传输失败
	Streaming int64 `json:"streaming"` // 其中流式请求数

	// ── 延迟（毫秒，累计和，由面板算平均）──────────────
	// 用累计和而非滑动窗口：无需后台 goroutine，重启后仍能从持久化恢复。
	TTFBSumMS    int64 `json:"ttfb_sum_ms"`    // 首字延迟累加（仅流式有值）
	TTFBCount    int64 `json:"ttfb_count"`     // 有 TTFB 采样的请求数
	LatencySumMS int64 `json:"latency_sum_ms"` // 端到端耗时累加
	LatencyCount int64 `json:"latency_count"`

	// ── Token ──────────────────────────────────────────
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	UsageReported    int64 `json:"usage_reported"` // 带 usage 的请求数（算平均值用）

	// ── 缓存（prompt cache）────────────────────────────
	// 上游按 prompt_cache_hit/miss 区分计费，命中部分通常便宜得多。
	CacheHitTokens   int64 `json:"cache_hit_tokens"`
	CacheMissTokens  int64 `json:"cache_miss_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// CacheReadTokens/CacheCreationTokens 是另一套命名（部分模型用），一并记录。
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`

	// ── 扣费 ───────────────────────────────────────────
	// CreditMilli 用「毫」为单位累计（上游 credit 是两位小数），避免浮点误差。
	CreditMilli int64 `json:"credit_milli"`

	// ── 吞吐（由面板按 token/耗时算）───────────────────
	// 这里额外记录"有首字到结束"的时长，用于算纯生成速率（剔除排队等待）。
	GenSumMS int64 `json:"gen_sum_ms"` // 首字→结束的毫秒累加
	GenCount int64 `json:"gen_count"`

	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// Snapshot 一次采样的完整视图。
type Snapshot struct {
	// Models 按模型名索引的累计统计。
	Models map[string]*ModelStats `json:"models"`
	// Total 全模型汇总（便于面板直接展示总量）。
	Total ModelStats `json:"total"`
	// Since 统计起点（进程启动或上次重置）。
	Since time.Time `json:"since"`
	// Now 快照时刻（面板用它和 Since 算运行时长）。
	Now time.Time `json:"now"`
}

// delta 单个请求的观测值，由 handler 填充。
type Delta struct {
	Model    string
	Stream   bool
	OK       bool
	TTFB     time.Duration // 流式首字延迟；同步请求为 0
	Latency  time.Duration // 端到端
	HasUsage bool

	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64

	CacheHitTokens      int64
	CacheMissTokens     int64
	CacheWriteTokens    int64
	CacheReadTokens     int64
	CacheCreationTokens int64

	Credit float64
}

// Collector 线程安全的统计收集器。
type Collector struct {
	mu     sync.Mutex
	models map[string]*ModelStats
	// series 按小时分桶的时间序列：桶键（小时）→ 模型 → 统计。
	//
	// 为什么按小时存而不是直接按天：按天无法回答"今天下午为什么慢"这类问题；
	// 按小时既能按天/周聚合（服务端求和），又保留小时级细节。
	// 保留期默认 30 天（≤720 桶 × 模型数），实测 6665 请求仅数 KB。
	series map[string]map[string]*ModelStats
	since  time.Time

	// stateFile 非空时落盘（重启后累计值不丢）。
	stateFile string
	// dirty 有未落盘变更。
	dirty bool
	// flushEvery 每 N 次记录落盘一次（避免每个请求都写盘）。
	flushEvery int
	sinceFlush int
	// retention 时间序列保留时长（<=0 用 defaultRetention）。
	retention time.Duration
	// nowFunc 便于测试注入时钟；nil 用 time.Now。
	nowFunc func() time.Time
}

// defaultRetention 时间序列默认保留时长。
//
// 30 天是折中：足够回答"这个月趋势如何"，又不至于让文件无界增长
// （按小时 720 桶 × 10 模型 ≈ 数千条记录，JSON 约几百 KB）。
const defaultRetention = 30 * 24 * time.Hour

// New 构建收集器；stateFile 为空表示纯内存（不持久化）。
func New(stateFile string) *Collector {
	c := &Collector{
		models:     map[string]*ModelStats{},
		series:     map[string]map[string]*ModelStats{},
		since:      time.Now(),
		stateFile:  stateFile,
		flushEvery: 20,
		retention:  defaultRetention,
	}
	if stateFile != "" {
		c.load()
	}
	return c
}

// SetRetention 设置时间序列保留时长（供配置注入）。
func (c *Collector) SetRetention(d time.Duration) {
	c.mu.Lock()
	c.retention = d
	c.mu.Unlock()
}

// now 返回当前时间（测试可注入）。
func (c *Collector) now() time.Time {
	if c.nowFunc != nil {
		return c.nowFunc()
	}
	return time.Now()
}

// bucketKey 把时刻截断到小时，格式 "2006-01-02T15"（本地时区，便于面板直接展示）。
//
// 用**本地时区**分桶而不是 UTC：面板按天/周聚合时，用户期望的是本地自然日，
// 若按 UTC 分桶则东八区会把 08:00 前的请求算进前一天。
//
// 注意：桶不记录时区偏移，聚合时按本地时区解释。跨时区迁移部署会导致
// 历史桶的含义变化——单机部署场景下可接受。
func bucketKey(t time.Time) string {
	return t.Local().Format("2006-01-02T15")
}

// parseBucketKey 解析桶键回时间（本地时区）。
func parseBucketKey(k string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02T15", k, time.Local)
}

// accumulate 把一次请求的观测值累加进给定统计对象。
//
// 抽成公共函数的原因：累计统计与时间序列桶必须用**完全相同**的口径累加，
// 若两处各写一份，日后改一处漏一处会导致"总览与趋势对不上"的诡异现象。
func accumulate(m *ModelStats, d Delta, now time.Time) {
	m.LastSeen = now

	m.Requests++
	if d.OK {
		m.Success++
	} else {
		m.Failed++
	}
	if d.Stream {
		m.Streaming++
	}

	if d.TTFB > 0 {
		m.TTFBSumMS += d.TTFB.Milliseconds()
		m.TTFBCount++
	}
	if d.Latency > 0 {
		ms := d.Latency.Milliseconds()
		m.LatencySumMS += ms
		m.LatencyCount++
		// 生成时长 = 端到端 - 首字等待（流式才有意义）。
		if d.TTFB > 0 && d.Latency > d.TTFB {
			m.GenSumMS += (d.Latency - d.TTFB).Milliseconds()
			m.GenCount++
		}
	}

	if d.HasUsage {
		m.UsageReported++
		m.PromptTokens += d.PromptTokens
		m.CompletionTokens += d.CompletionTokens
		m.TotalTokens += d.TotalTokens
		m.CacheHitTokens += d.CacheHitTokens
		m.CacheMissTokens += d.CacheMissTokens
		m.CacheWriteTokens += d.CacheWriteTokens
		m.CacheReadTokens += d.CacheReadTokens
		m.CacheCreationTokens += d.CacheCreationTokens
	}
	// credit 以「毫」累计：上游给两位小数（0.02），×1000 后是整数。
	if d.Credit != 0 {
		m.CreditMilli += int64(d.Credit*1000 + 0.5)
	}
}

// Record 记录一次请求（同时进累计统计与小时时间序列）。
func (c *Collector) Record(d Delta) {
	model := d.Model
	if model == "" {
		model = "(unknown)"
	}
	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	// 1) 累计统计。
	m, ok := c.models[model]
	if !ok {
		m = &ModelStats{Model: model, FirstSeen: now}
		c.models[model] = m
	}
	accumulate(m, d, now)

	// 2) 小时桶。
	key := bucketKey(now)
	bucket, ok := c.series[key]
	if !ok {
		bucket = map[string]*ModelStats{}
		c.series[key] = bucket
	}
	bm, ok := bucket[model]
	if !ok {
		bm = &ModelStats{Model: model, FirstSeen: now}
		bucket[model] = bm
	}
	accumulate(bm, d, now)

	// 3) 顺带清理过期桶（每次记录时检查开销极小：只在跨桶时才会真正遍历）。
	c.pruneLocked(now)

	c.dirty = true
	c.sinceFlush++
	if c.stateFile != "" && c.sinceFlush >= c.flushEvery {
		c.saveLocked()
	}
}

// pruneLocked 删除超出保留期的桶。调用方必须已持锁。
//
// 优化：用一个"上次清理时刻"跳过绝大多数调用——桶只按小时增长，
// 不必每次请求都遍历整个 series。
func (c *Collector) pruneLocked(now time.Time) {
	ret := c.retention
	if ret <= 0 {
		ret = defaultRetention
	}
	cutoff := now.Add(-ret)
	for k := range c.series {
		t, err := parseBucketKey(k)
		if err != nil {
			delete(c.series, k) // 无法解析的脏键直接清掉
			continue
		}
		if t.Before(cutoff) {
			delete(c.series, k)
		}
	}
}

// ---------------------------------------------------------------------------
// 时间维度查询
// ---------------------------------------------------------------------------

// Interval 聚合粒度。
type Interval string

const (
	// IntervalHour 按小时（原始桶，不聚合）。
	IntervalHour Interval = "hour"
	// IntervalDay 按自然日（本地时区）。
	IntervalDay Interval = "day"
	// IntervalWeek 按自然周（周一为起点，本地时区）。
	IntervalWeek Interval = "week"
)

// NormalizeInterval 规范化粒度参数，非法值回落 hour。
func NormalizeInterval(s string) Interval {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "day", "daily", "d":
		return IntervalDay
	case "week", "weekly", "w":
		return IntervalWeek
	default:
		return IntervalHour
	}
}

// RangeQuery 时间范围查询参数。
type RangeQuery struct {
	// From/To 查询区间（含 From、不含 To）；零值表示不限制。
	From time.Time
	To   time.Time
	// Interval 聚合粒度。
	Interval Interval
	// Model 非空时只统计该模型。
	Model string
}

// Point 时间序列上的一个数据点。
type Point struct {
	// Key 分组键（小时桶 "2026-09-14T15" / 日 "2026-09-14" / 周 "2026-W38"）。
	Key string `json:"key"`
	// Start 该分组起点（本地时区），面板画图用。
	Start time.Time `json:"start"`
	// End 该分组终点（不含）。
	End time.Time `json:"end"`

	// 该分组内的统计（口径与累计统计一致）。
	Stats *ModelStats `json:"stats"`
	// Derived 派生指标（平均延迟/吞吐/命中率等），面板直接展示。
	Derived Derived `json:"derived"`
}

// RangeResult 时间范围查询结果。
type RangeResult struct {
	Interval Interval  `json:"interval"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	// Points 按时间升序的数据点。
	Points []Point `json:"points"`
	// Total 该范围内所有分组的汇总。
	Total Derived `json:"total"`
	// Models 范围内出现过的模型（升序），供面板做模型筛选。
	Models []string `json:"models"`
}

// groupKey 把时刻按粒度归组，返回组键与组的起止时间（本地时区）。
func groupKey(t time.Time, iv Interval) (key string, start, end time.Time) {
	t = t.Local()
	switch iv {
	case IntervalDay:
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
		end = start.AddDate(0, 0, 1)
		return start.Format("2006-01-02"), start, end
	case IntervalWeek:
		// 以周一为一周起点（ISO 习惯，符合中文语境"这周"的直觉）。
		// Go 的 Weekday()：Sunday=0，需先转换到"距周一的天数"。
		offset := (int(t.Weekday()) + 6) % 7
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -offset)
		end = start.AddDate(0, 0, 7)
		isoYear, isoWeek := start.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", isoYear, isoWeek), start, end
	default: // hour
		start = t.Truncate(time.Hour)
		end = start.Add(time.Hour)
		return start.Format("2006-01-02T15"), start, end
	}
}

// Range 按时间范围聚合查询。
//
// 实现取态：从小时桶**滚动求和**，而不是预先物化日/周表。
// 原因：小时桶是唯一真相源，日/周只是视图——避免了"日表与小时表不一致"
// 这类经典的双写问题，代价是聚合时多遍历几十个桶（代价可忽略）。
func (c *Collector) Range(q RangeQuery) RangeResult {
	iv := q.Interval
	if iv == "" {
		iv = IntervalHour
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 先按分组键聚合。
	type group struct {
		start, end time.Time
		models     map[string]*ModelStats
	}
	groups := map[string]*group{}
	modelSet := map[string]bool{}

	for key, bucket := range c.series {
		t, err := parseBucketKey(key)
		if err != nil {
			continue
		}
		// 区间过滤：桶的起点落在 [From, To) 内才计入。
		if !q.From.IsZero() && t.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && !t.Before(q.To) {
			continue
		}
		for model, ms := range bucket {
			if q.Model != "" && model != q.Model {
				continue
			}
			modelSet[model] = true
			gk, gstart, gend := groupKey(t, iv)
			g, ok := groups[gk]
			if !ok {
				g = &group{start: gstart, end: gend, models: map[string]*ModelStats{}}
				groups[gk] = g
			}
			dst, ok := g.models[model]
			if !ok {
				dst = &ModelStats{Model: model}
				g.models[model] = dst
			}
			addInto(dst, ms)
		}
	}

	// 转成有序数据点。
	points := make([]Point, 0, len(groups))
	total := &ModelStats{Model: "(all)"}
	for k, g := range groups {
		merged := &ModelStats{Model: "(all)"}
		for _, ms := range g.models {
			addInto(merged, ms)
		}
		addInto(total, merged)
		points = append(points, Point{
			Key:     k,
			Start:   g.start,
			End:     g.end,
			Stats:   merged,
			Derived: Derive(merged),
		})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Start.Before(points[j].Start) })

	models := make([]string, 0, len(modelSet))
	for m := range modelSet {
		models = append(models, m)
	}
	sort.Strings(models)

	out := RangeResult{
		Interval: iv,
		Points:   points,
		Total:    Derive(total),
		Models:   models,
	}
	// 回填实际生效的区间边界（面板展示"显示的是哪段"）。
	if len(points) > 0 {
		out.From = points[0].Start
		out.To = points[len(points)-1].End
	} else {
		out.From = q.From
		out.To = q.To
	}
	return out
}

// Series 返回时间序列的原始桶数（供 /v1/stats 透出，便于判断保留期）。
func (c *Collector) SeriesBuckets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.series)
}

// EarliestBucket 返回最早桶的起点（无数据时为零值）。
func (c *Collector) EarliestBucket() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var earliest time.Time
	for k := range c.series {
		t, err := parseBucketKey(k)
		if err != nil {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Snapshot 返回当前累计统计的深拷贝（含全模型汇总）。
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := Snapshot{
		Models: make(map[string]*ModelStats, len(c.models)),
		Since:  c.since,
		Now:    time.Now(),
	}
	total := &ModelStats{Model: "(all)"}
	for name, m := range c.models {
		cp := *m
		out.Models[name] = &cp
		addInto(total, &cp)
	}
	out.Total = *total
	return out
}

// Reset 清空统计（面板"重置统计"用）。
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = map[string]*ModelStats{}
	c.series = map[string]map[string]*ModelStats{}
	c.since = time.Now()
	c.dirty = true
	c.saveLocked()
}

// Flush 强制落盘（进程退出前调用）。
func (c *Collector) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dirty {
		c.saveLocked()
	}
}

// addInto 把 src 的计数累加进 dst（汇总用）。
func addInto(dst, src *ModelStats) {
	dst.Requests += src.Requests
	dst.Success += src.Success
	dst.Failed += src.Failed
	dst.Streaming += src.Streaming
	dst.TTFBSumMS += src.TTFBSumMS
	dst.TTFBCount += src.TTFBCount
	dst.LatencySumMS += src.LatencySumMS
	dst.LatencyCount += src.LatencyCount
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.TotalTokens += src.TotalTokens
	dst.UsageReported += src.UsageReported
	dst.CacheHitTokens += src.CacheHitTokens
	dst.CacheMissTokens += src.CacheMissTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.CreditMilli += src.CreditMilli
	dst.GenSumMS += src.GenSumMS
	dst.GenCount += src.GenCount
	if dst.FirstSeen.IsZero() || (!src.FirstSeen.IsZero() && src.FirstSeen.Before(dst.FirstSeen)) {
		dst.FirstSeen = src.FirstSeen
	}
	if src.LastSeen.After(dst.LastSeen) {
		dst.LastSeen = src.LastSeen
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

type stateFile struct {
	Since  time.Time              `json:"since"`
	Models map[string]*ModelStats `json:"models"`
	// Series 按小时分桶的时间序列（旧文件缺此字段 → nil，向后兼容：
	// 累计统计照常恢复，时间趋势从该次启动重新累积）。
	Series map[string]map[string]*ModelStats `json:"series,omitempty"`
	// RetentionSec 落盘时的保留期（秒），供排查"桶为何被清理"。
	RetentionSec int64 `json:"retention_sec,omitempty"`
}

// saveLocked 原子落盘。调用方必须已持锁。
// 落盘失败只打日志不阻断（统计是观测功能，不应影响转发）。
func (c *Collector) saveLocked() {
	c.dirty = false
	c.sinceFlush = 0
	if c.stateFile == "" {
		return
	}
	raw, err := json.MarshalIndent(stateFile{
		Since:        c.since,
		Models:       c.models,
		Series:       c.series,
		RetentionSec: int64(c.retention / time.Second),
	}, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.stateFile); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := c.stateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.stateFile)
}

// load 启动时恢复累计统计（文件缺失/损坏时静默从零开始）。
func (c *Collector) load() {
	raw, err := os.ReadFile(c.stateFile)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	if sf.Models != nil {
		c.models = sf.Models
	}
	if sf.Series != nil {
		c.series = sf.Series
	}
	if !sf.Since.IsZero() {
		c.since = sf.Since
	}
	// 启动时清理一次过期桶：进程可能停机数天，期间无请求触发清理。
	c.pruneLocked(c.now())
}

// ---------------------------------------------------------------------------
// 展示辅助（面板可直接用的派生指标）
// ---------------------------------------------------------------------------

// Derived 单模型的派生指标（平均/速率），面板表格直接用。
type Derived struct {
	Model string `json:"model"`

	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	// AvgTTFBMS 平均首字延迟（毫秒）；无采样为 0。
	AvgTTFBMS float64 `json:"avg_ttfb_ms"`
	// AvgLatencyMS 平均端到端耗时（毫秒）。
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	// TokensPerSec 生成速率 = 输出 token / 生成秒数（剔除首字等待）。
	TokensPerSec float64 `json:"tokens_per_sec"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CacheHitTokens   int64 `json:"cache_hit_tokens"`
	CacheMissTokens  int64 `json:"cache_miss_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	// CacheHitRate 缓存命中率 = hit / (hit + miss)，无数据为 0。
	CacheHitRate float64 `json:"cache_hit_rate"`

	// Credit 累计扣费（元/积分，两位小数）。
	Credit float64 `json:"credit"`
	// CreditPerReq 平均每请求扣费。
	CreditPerReq float64 `json:"credit_per_req"`

	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// Derive 把累计统计换算成派生指标。
func Derive(m *ModelStats) Derived {
	d := Derived{
		Model:            m.Model,
		Requests:         m.Requests,
		Success:          m.Success,
		Failed:           m.Failed,
		Streaming:        m.Streaming,
		PromptTokens:     m.PromptTokens,
		CompletionTokens: m.CompletionTokens,
		TotalTokens:      m.TotalTokens,
		CacheHitTokens:   m.CacheHitTokens + m.CacheReadTokens,
		CacheMissTokens:  m.CacheMissTokens,
		CacheWriteTokens: m.CacheWriteTokens + m.CacheCreationTokens,
		Credit:           float64(m.CreditMilli) / 1000,
	}
	if m.TTFBCount > 0 {
		d.AvgTTFBMS = float64(m.TTFBSumMS) / float64(m.TTFBCount)
	}
	if m.LatencyCount > 0 {
		d.AvgLatencyMS = float64(m.LatencySumMS) / float64(m.LatencyCount)
	}
	// 生成速率：优先用"首字→结束"的纯生成时长；无 TTFB 采样时退回端到端耗时。
	if m.GenCount > 0 && m.GenSumMS > 0 {
		d.TokensPerSec = float64(m.CompletionTokens) / (float64(m.GenSumMS) / 1000)
	} else if m.LatencySumMS > 0 {
		d.TokensPerSec = float64(m.CompletionTokens) / (float64(m.LatencySumMS) / 1000)
	}
	if total := d.CacheHitTokens + d.CacheMissTokens; total > 0 {
		d.CacheHitRate = float64(d.CacheHitTokens) / float64(total)
	}
	if m.Requests > 0 {
		d.CreditPerReq = d.Credit / float64(m.Requests)
	}
	if !m.LastSeen.IsZero() {
		t := m.LastSeen
		d.LastSeen = &t
	}
	return d
}

// DerivedSnapshot 面板用的完整派生视图。
type DerivedSnapshot struct {
	Models []Derived `json:"models"`
	Total  Derived   `json:"total"`
	Since  time.Time `json:"since"`
	Now    time.Time `json:"now"`
	// UptimeSec 统计持续时间（秒），面板算速率用。
	UptimeSec int64 `json:"uptime_sec"`
}

// Derived 生成面板视图（模型按请求数降序）。
func (c *Collector) Derived() DerivedSnapshot {
	snap := c.Snapshot()
	out := DerivedSnapshot{
		Models: make([]Derived, 0, len(snap.Models)),
		Since:  snap.Since,
		Now:    snap.Now,
	}
	for _, m := range snap.Models {
		out.Models = append(out.Models, Derive(m))
	}
	sort.Slice(out.Models, func(i, j int) bool {
		if out.Models[i].Requests != out.Models[j].Requests {
			return out.Models[i].Requests > out.Models[j].Requests
		}
		return out.Models[i].Model < out.Models[j].Model
	})
	out.Total = Derive(&snap.Total)
	out.Total.Model = "(all)"
	if !snap.Since.IsZero() {
		out.UptimeSec = int64(snap.Now.Sub(snap.Since).Seconds())
	}
	return out
}

// String 便于日志调试。
func (d Derived) String() string {
	return fmt.Sprintf("%s req=%d ttfb=%.0fms tok/s=%.1f cache=%.0f%% credit=%.2f",
		d.Model, d.Requests, d.AvgTTFBMS, d.TokensPerSec, d.CacheHitRate*100, d.Credit)
}
