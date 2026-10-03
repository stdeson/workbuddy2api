//go:build spreadprobe

// 一次性诊断：用线上真实 config.json + 真实 auths 目录，演算铺开后的账号时序。
// 用 `go test -tags spreadprobe -run TestProbeRealDeployment -v` 跑，跑完即可删。
package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/config"
	"workbuddy2api/internal/pool"
)

func TestProbeRealDeployment(t *testing.T) {
	root := os.Getenv("PROBE_ROOT")
	if root == "" {
		t.Skip("PROBE_ROOT 未设置")
	}
	raw, err := os.ReadFile(root + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	// 走与线上一致的路径：DefaultSchedule 打底 → Unmarshal 覆盖 → Normalize。
	// 根类型在 cmd/server，这里只取 schedule 段（等价路径）。
	var wrapper struct {
		Schedule json.RawMessage `json:"schedule"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatal(err)
	}
	sch := config.DefaultSchedule()
	if err := json.Unmarshal(wrapper.Schedule, &sch); err != nil {
		t.Fatal(err)
	}
	if err := sch.Normalize(); err != nil {
		t.Fatal(err)
	}
	sp := sch.Spread
	fmt.Printf("\n=== 线上 config 解析出的 schedule.spread ===\n%+v\n\n", sp)

	// 真实账号池
	as, err := auth.LoadDir(root + "/auths")
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	for _, a := range as {
		p.Add(a)
	}
	sts := p.List()
	fmt.Printf("=== 真实账号池：%d 个号 ===\n", len(sts))

	names := map[string]string{}
	for _, st := range sts {
		names[st.UID] = st.Nickname
	}

	cfg := Config{
		Pool:                 p,
		RandomWindowEnabled:  true,
		RandomWindowStartMin: 8*60 + 33,
		RandomWindowEndMin:   23*60 + 25,
		ActivityReportCount:  sch.ActivityReportCount,
		AccountSpread:        time.Duration(sp.AccountSpreadMin) * time.Minute,
		AccountJitter:        time.Duration(sp.AccountJitterSec) * time.Second,
		MinAccountDelay:      time.Duration(sp.MinAccountDelaySec) * time.Second,
		MaxAccountDelay:      time.Duration(sp.MaxAccountDelaySec) * time.Second,
		MinReportGap:         time.Duration(sp.MinReportGapSec) * time.Second,
		MaxReportGap:         time.Duration(sp.MaxReportGapSec) * time.Second,
		ShuffleAccounts:      sp.ShuffleAccounts,
	}
	s := New(cfg)

	fmt.Printf("\n=== batchReserve（整批墙钟上界）===\n  %v\n", s.batchReserve())
	resMin := int((s.batchReserve() + time.Minute - 1) / time.Minute)
	fmt.Printf("  随机窗口 08:33~23:25 → 实际基准区间 08:33 ~ %02d:%02d\n",
		(23*60+25-resMin)/60, (23*60+25-resMin)%60)

	for _, kind := range []string{"checkin", "travel", "activity"} {
		type row struct {
			uid, nick string
			off       time.Duration
		}
		rows := make([]row, 0, len(sts))
		uniq := map[time.Duration]bool{}
		for _, st := range sts {
			off := s.accountOffset(st.UID, kind)
			uniq[off] = true
			rows = append(rows, row{st.UID[:8], names[st.UID], off})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].off < rows[j].off })
		fmt.Printf("\n=== %s 族：%d 个号的稳定日偏移（%d 个不同时刻）===\n", kind, len(rows), len(uniq))
		for _, r := range rows {
			fmt.Printf("  %s  %-8s  +%d分%02d秒\n", r.uid, r.nick,
				int(r.off.Minutes()), int(r.off.Seconds())%60)
		}
		if len(uniq) < len(rows) {
			fmt.Printf("  ⚠️ 有 %d 个号撞在同一时刻\n", len(rows)-len(uniq))
		}
	}

	// 对比：同一天内相邻两号的时间差（偏移 + 账号间随机间隔）
	fmt.Printf("\n=== 铺开后同族相邻两号的间隔（含账号间随机延迟，抽样 200 轮）===\n")
	for _, kind := range []string{"checkin", "travel"} {
		var gaps []time.Duration
		for i := 0; i < 200; i++ {
			for _, a := range sts {
				for _, b := range sts {
					if a.UID == b.UID {
						continue
					}
					d := s.accountOffset(b.UID, kind) - s.accountOffset(a.UID, kind)
					if d <= 0 {
						continue
					}
					gaps = append(gaps, d)
				}
			}
		}
		sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
		if len(gaps) == 0 {
			continue
		}
		fmt.Printf("  %-9s 最小 %v  中位 %v  最大 %v\n", kind,
			gaps[0].Round(time.Second), gaps[len(gaps)/2].Round(time.Second),
			gaps[len(gaps)-1].Round(time.Second))
	}
}
