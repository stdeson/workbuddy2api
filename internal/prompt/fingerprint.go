package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// SystemFingerprint 摘要「这条请求自带的 system/developer 文本」。
//
// 存在的唯一理由：降级闸门必须按**指纹**判定，不能按「今天有没有谁被拦过」判定。
//
// 2026-10-21 事故：passthrough 模式下，另一个客户端（DSH/Claude Code 体）的 system
// 被上游 400 content_blocked（11-128）误判后，降级闸门被**全局**打开到次日 00:00，
// 于是当天**所有**客户端、所有账号的请求，system 都被换成了 Degraded 中性提示词
// （"You are a helpful assistant."）。对 tian_yan「问AI」这类把行情事实放在 system
// 里的客户端，等于**把 1643 字的【标的】+【当日行情】+【公司画像】整块静默丢掉**，
// 界面上显示「本次提示词」完好、模型却反问「你说的是哪家公司？」。
//
// 指纹 = 该请求所有 system/developer 消息文本（按出现顺序、\x00 分隔）的 sha256 前 8 字节。
// 语义边界：
//   - 没有任何 system/developer 消息 → 返回空串（调用方据此**不参与降级**：
//     没有 system 可换，降级重试只会凭空塞一条提示词，改变语义却解决不了问题）；
//   - 坏 JSON / 无 messages → 空串（与 Rewrite/Append 的守卫一致：**绝不失败**）；
//   - content 既接受字符串，也接受块数组（只取 type=text 的 text 段）。
func SystemFingerprint(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		texts = append(texts, contentText(mm["content"]))
	}
	if len(texts) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(texts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// contentText 把一条消息的 content 拍成纯文本：字符串原样，块数组只取 text 段。
func contentText(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			blk, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := blk["type"].(string); typ != "" && typ != "text" {
				continue
			}
			if text, ok := blk["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}
