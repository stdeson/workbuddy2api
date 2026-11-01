// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"time"

	"workbuddy2api/internal/auth"
)

// PickExcludingForRealm 按 realm 过滤的轮换选号：候选仅限 Realm()==realm 的账号。
// realm=="" 退化为 PickExcluding（现状语义，老调用零改动）。
// 可选做请求级轮换（tried）与模型感知（reqModel，6004 模型豁免照常生效）；
// reqModel 非空时健康口径换成 healthyForModel。realm 不匹配的全冷却兜底同样排除。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// PickRecentSuccessForRealm 只在「最近成功过」（lastSuccess 晚于 within 之前）
// 的账号里选号：用于 11140 全局封禁风暴期——优先用**已验证健康**的号服务，
// 不去试探尚未验证的号（避免把未知的号一个个送上去撞封）。
//
// 无候选时返回 nil（**不走**全冷却兜底）：调用方据此决定是否退回普通选号。
// within <= 0 退化为普通选号（PickExcludingForRealm 语义）。
func (p *Pool) PickRecentSuccessForRealm(tried map[string]bool, reqModel, realm string, within time.Duration) *auth.Auth {
	if within <= 0 {
		return p.PickExcludingForRealm(tried, reqModel, realm)
	}
	return p.pickRecentSuccess(tried, reqModel, realm, time.Now().Add(-within))
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// DeptestOnly: 仅 realm_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm。保留作 ForModelRealm 的模型维度退化
// （model=""）语义锚点测试。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsLocked(realm, func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.availableUIDsLocked(realm,
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}
