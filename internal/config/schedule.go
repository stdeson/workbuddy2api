// Package config 存放跨命令共享的配置段与默认值逻辑。
//
// 起因 issue #49：cmd/activity 一次性触发器曾自复制一份精简 schedule 结构体，
// 只 json.Unmarshal 无默认值，键缺席 → Go 零值 0 → scheduler 归一为 1，
// 与 cmd/server 主程序缺省 5 条漂移。把 Schedule 段 + 默认值/归一化抽到本包，
// 两个命令共用同一份定义，消除漂移源头。
package config

import "fmt"

// Schedule 排程配置段（对应 config.json 的 "schedule" 对象）。
//
// 六类独立排程：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子。
// cmd/server 与 cmd/activity 共用本结构，默认值由 DefaultSchedule 填充、
// 缺省归一由 Normalize 完成——两命令走同一份语义，不再各自复制。
type Schedule struct {
	CheckinHours   []int `json:"checkin_hours"`   // [9,21]
	TravelHours    []int `json:"travel_hours"`    // [9,21]
	ActivityHours  []int `json:"activity_hours"`  // [10]
	KeepaliveHours []int `json:"keepalive_hours"` // [22]
	SchoolHours    []int `json:"school_hours"`    // [12] 开学季任务（迁移自 school/cat 两条系统 crontab）
	CatHours       []int `json:"cat_hours"`       // [1] 夜猫窗口 23-08 CST，01:00 窗口内补 1 次
	// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/SchoolEnabled/CatEnabled
	// 显式禁用开关（缺省 true）。
	//
	// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
	//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
	//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
	//     则对老配置零影响，向后完全兼容。
	//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
	//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
	CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
	TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
	ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
	KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
	SchoolEnabled    bool `json:"school_enabled"`    // 缺省 true；false = 停开学季任务
	CatEnabled       bool `json:"cat_enabled"`       // 缺省 true；false = 停夜猫子任务
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int `json:"activity_report_count"`
	// 猫猫旅行已退役 travel_interval_minutes：旅行现为独立排程（travel_hours）。
	// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。

	// RandomWindow 随机每日窗口：启用后，签到/旅行/活跃上报/token 保活 四类每日维护任务
	// 合并为每天一次，在 [Start,End] 之间的随机整分时刻触发（防账号行为规律被检测）。
	// 启用时忽略上面四类的 *_Hours 固定整点排程；开学季/夜猫子仍走各自固定小时。
	// start/end 格式 "HH:MM"（本地 CST），需满足 00:00<=start<end<=23:59。
	RandomWindowEnabled bool   `json:"random_window_enabled"`
	RandomWindowStart   string `json:"random_window_start"` // 例 "08:33"
	RandomWindowEnd     string `json:"random_window_end"`   // 例 "23:25"

	// 开学季随机时点：启用后开学季任务每天在 [SchoolRandomStart,SchoolRandomEnd] 随机整分
	// 触发一次（替代固定 school_hours）。窗内不跨午夜，需满足 start<end。
	// 缺省窗口 08:00~11:00（用户要求：避开核心随机窗口 08:33 起点，落在上午）。
	SchoolRandomEnabled bool   `json:"school_random_enabled"`
	SchoolRandomStart   string `json:"school_random_start"` // 缺省 "08:00"
	SchoolRandomEnd     string `json:"school_random_end"`   // 缺省 "11:00"
	// 夜猫子随机时点：启用后夜猫子任务每天在 [CatRandomStart,CatRandomEnd] 窗口内随机生成
	// CatRandomCount 个时刻触发（替代固定 cat_hours）。窗口允许跨午夜（如 23:00~02:00：
	// end<start 表示跨日，时长 = (end-start+1440)%1440）。夜猫窗口 23:00-08:00 硬约束仍在，
	// 随机时刻必须落在该窗口内才能领奖，故 CatRandomStart/End 应包在 23:00~08:00 中。
	// 缺省 23:00~02:00、每天 3 次（用户要求：分散触发降检测，但服务端每窗口至多补 1 次，
	// 故仅首个随机时刻真正领奖，其余为幂等 no-op）。
	CatRandomEnabled bool   `json:"cat_random_enabled"`
	CatRandomStart   string `json:"cat_random_start"` // 缺省 "23:00"
	CatRandomEnd     string `json:"cat_random_end"`   // 缺省 "02:00"
	CatRandomCount   int    `json:"cat_random_count"` // 缺省 3；<=0 归一为 3
}

// DefaultSchedule 返回排程段的默认值。
//
// 开关「缺省 true」靠这里实现：调用方先取 DefaultSchedule 再用 json.Unmarshal 覆盖，
// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
// ActivityReportCount 默认 5：领猫前置需 5 次对话，5 连发刷满 chat_5。
func DefaultSchedule() Schedule {
	return Schedule{
		CheckinHours:        []int{9, 21},
		TravelHours:         []int{9, 21},
		ActivityHours:       []int{10},
		KeepaliveHours:      []int{22},
		SchoolHours:         []int{12},
		CatHours:            []int{1},
		CheckinEnabled:      true,
		TravelEnabled:       true,
		ActivityEnabled:     true,
		KeepaliveEnabled:    true,
		SchoolEnabled:       true,
		CatEnabled:          true,
		ActivityReportCount: 5, // 领猫前置需 5 次对话，5 连发刷满 chat_5
		// 开学季/夜猫子随机时点缺省关闭（回落固定小时），由 config 显式开启。
		SchoolRandomEnabled: false,
		SchoolRandomStart:   "08:00",
		SchoolRandomEnd:     "11:00",
		CatRandomEnabled:    false,
		CatRandomStart:      "23:00",
		CatRandomEnd:        "02:00",
		CatRandomCount:      3,
	}
}

// Normalize 归一化排程段：空数组/null 回落默认小时，ActivityReportCount 归一，校验小时范围。
//
// 空数组与 null 反序列化后覆盖掉 DefaultSchedule 的排程值（键缺席才保留），在此补齐。
// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
//
// ActivityReportCount：0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
// 注意这是「显式配 0 = 旧行为」的兼容语义，与 scheduler.New 的 <=0 → 1 归一一致；
// 「缺省 = 5」由 DefaultSchedule 在 Unmarshal 前置入，是另一条路径，两者不合并。
func (s *Schedule) Normalize() error {
	if len(s.CheckinHours) == 0 {
		s.CheckinHours = []int{9, 21}
	}
	if len(s.TravelHours) == 0 {
		s.TravelHours = []int{9, 21}
	}
	if len(s.ActivityHours) == 0 {
		s.ActivityHours = []int{10}
	}
	if len(s.KeepaliveHours) == 0 {
		s.KeepaliveHours = []int{22}
	}
	if len(s.SchoolHours) == 0 {
		s.SchoolHours = []int{12}
	}
	if len(s.CatHours) == 0 {
		s.CatHours = []int{1}
	}
	// 0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if s.ActivityReportCount <= 0 {
		s.ActivityReportCount = 1
	}
	// 夜猫子随机次数：<=0 归一为 3（缺省每天 3 次）。
	if s.CatRandomCount <= 0 {
		s.CatRandomCount = 3
	}
	return s.validateHours()
}

// validateHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (s *Schedule) validateHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", s.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", s.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", s.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", s.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.school_hours", "school_enabled", s.SchoolHours); err != nil {
		return err
	}
	return checkHourRange("schedule.cat_hours", "cat_enabled", s.CatHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
