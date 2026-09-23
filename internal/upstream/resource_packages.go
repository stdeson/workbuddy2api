// resource_packages.go get-user-resource 的「全量套餐明细」口径（/v1/quota 数据源）。
//
// 与 getUserResourceBody（UserResourceDetailed / ResourceSummary 共用）的差异：
// 那个口径带 ProductCode/Status 过滤，语义是「服务余额聚合」，历史逐字不可动；
// 本口径**不带任何过滤** —— 实测过滤会把部分账号的赠送/奖励包隐藏掉，而配额看板
// 恰恰要看到全量套餐（含 Complimentary bag）。
//
// 来源：社区 fork 287775856/workbuddy2api（现 linbeize/workbuddy2api）的
// internal/upstream/resource.go，按本仓库的 billingMeterJSON（realm 感知 + 候选路径
// 404 fallback）与 packageRemainUsed（单一事实来源，含 remain 钳位）重写。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"

	"workbuddy2api/internal/auth"
)

// ResourcePackage 单个积分套餐（配额看板形状：used/total/remaining/resetAt）。
// JSON 标签与社区面板/9Router 看板约定一致，勿改。
type ResourcePackage struct {
	PackageName         string `json:"packageName"`
	CycleCapacitySize   int64  `json:"total"`
	CycleCapacityUsed   int64  `json:"used"`
	CycleCapacityRemain int64  `json:"remaining"`
	CycleEndTime        string `json:"resetAt"`
	Recurring           bool   `json:"recurring"`
}

// ResourcePackages 拉取账号的**全部**积分套餐明细（不做 ProductCode/Status 过滤）。
// realm 感知继承 billingMeterPaths：global 账号打 workbuddy.ai（404 回落 /v2），
// CN 账号维持 /v2/billing/meter/get-user-resource。
//
// 单套餐的 remain/used/size 统一走 packageRemainUsed（与 UserResourceDetailed /
// ResourceSummary / cmd/credit 同源），避免出现第三套钳位口径。
func (c *Client) ResourcePackages(a *auth.Auth) ([]ResourcePackage, error) {
	body := map[string]any{
		"PageNumber": 1,
		"PageSize":   200,
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return nil, err
	}
	var resp userResourceResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("resource parse: %w", err)
	}
	packages := make([]ResourcePackage, 0, len(resp.Response.Data.Accounts))
	for _, acct := range resp.Response.Data.Accounts {
		remain, used, size := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		name := acct.PackageName
		if name == "" {
			name = "Credit Package"
		}
		packages = append(packages, ResourcePackage{
			PackageName:         name,
			CycleCapacitySize:   size,
			CycleCapacityUsed:   used,
			CycleCapacityRemain: remain,
			CycleEndTime:        acct.CycleEndTime,
			// Recurring = 周期型套餐（有 CycleCapacity 三字段）。与「一次性买断包」
			// 区分：前者按周期重置，看板渲染的 resetAt 才有意义。
			Recurring: acct.CycleCapacitySize > 0,
		})
	}
	return packages, nil
}
