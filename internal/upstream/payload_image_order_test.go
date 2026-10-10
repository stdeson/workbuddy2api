package upstream

import (
	"encoding/json"
	"testing"
)

// imgMsg 构造一条带图的 user 消息（含 DSH 风格的字符串与对象两种 image_url 形态）。
func imgMsg(text string) map[string]any {
	return map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/webp;base64,AAAA"}},
		},
	}
}

func txtMsg(role, text string) map[string]any {
	return map[string]any{"role": role, "content": text}
}

// TestHoistImageTurnLastMovesTrailingContext 核心回归：DSH 在图片消息之后追加注入块
// （AGENTS.md 提醒 / 运行时上下文 / 技能清单 / 时间戳 / 记忆），这些都算 user 消息。
// 上游 cn:hy3 只认最后一条 user 消息里的图，必须把图片轮次上浮到末尾。
func TestHoistImageTurnLastMovesTrailingContext(t *testing.T) {
	msgs := []any{
		txtMsg("system", "sys"),
		txtMsg("user", "<openviking-context>...index...</openviking-context>"),
		imgMsg("这是什么"),
		txtMsg("user", "<system-reminder>AGENTS.md</system-reminder>"),
		txtMsg("user", "Current runtime context."),
		txtMsg("user", "Time sampled ..."),
	}
	got := hoistImageTurnLast(msgs)
	if len(got) != len(msgs) {
		t.Fatalf("length changed: %d -> %d", len(msgs), len(got))
	}
	if !messageHasImagePart(got[len(got)-1]) {
		t.Fatal("image message must end up LAST")
	}
	// 其余消息相对顺序不变（按渲染后的顺序断言内容）
	want := []string{"sys", "<openviking-context>...index...</openviking-context>",
		"<system-reminder>AGENTS.md</system-reminder>", "Current runtime context.", "Time sampled ..."}
	for i, w := range want {
		if c, _ := got[i].(map[string]any)["content"].(string); c != w {
			t.Fatalf("position %d: want %q, got %q (order=%v)", i, w, c, dumpOrder(got))
		}
	}
}

// dumpOrder 渲染 messages 的顺序指纹，便于失败信息可读。
func dumpOrder(msgs []any) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if messageHasImagePart(m) {
			out = append(out, "<IMAGE>")
			continue
		}
		c, _ := mm["content"].(string)
		out = append(out, c)
	}
	return out
}

// TestHoistImageTurnLastNoop 不该搬的情形一律原样返回（同一个 slice）。
func TestHoistImageTurnLastNoop(t *testing.T) {
	cases := map[string][]any{
		"image already last": {
			txtMsg("system", "s"), imgMsg("q"),
		},
		"no image at all": {
			txtMsg("system", "s"), txtMsg("user", "hi"), txtMsg("user", "there"),
		},
		"assistant after image": {
			txtMsg("system", "s"), imgMsg("q"), txtMsg("assistant", "ans"), txtMsg("user", "more"),
		},
		"tool after image": {
			txtMsg("system", "s"), imgMsg("q"), txtMsg("tool", "{}"),
		},
		"another image after": {
			txtMsg("system", "s"), imgMsg("q1"), imgMsg("q2"),
		},
	}
	for name, msgs := range cases {
		before, _ := json.Marshal(msgs)
		got := hoistImageTurnLast(msgs)
		after, _ := json.Marshal(got)
		if string(before) != string(after) {
			t.Fatalf("%s: expected no-op ordering, got %v", name, dumpOrder(got))
		}
	}
}

// TestPrepareBodyHoistsImageTurn 端到端：走真实改写管线后，图片消息落在末尾，
// 且 image_url 已被归一为对象形态。
func TestPrepareBodyHoistsImageTurn(t *testing.T) {
	src := []byte(`{
		"model":"cn:hy3",
		"messages":[
			{"role":"system","content":"sys"},
			{"role":"user","content":[{"type":"text","text":"这是什么"},{"type":"image_url","image_url":"data:image/webp;base64,AAAA"}]},
			{"role":"user","content":"Current runtime context."}
		]
	}`)
	out := PrepareBodyOpt(src, false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	if !messageHasImagePart(msgs[2]) {
		t.Fatalf("image message must be last, got %v", msgs)
	}
	if msgs[1].(map[string]any)["content"] != "Current runtime context." {
		t.Fatalf("context block must precede the image turn: %v", msgs[1])
	}
	// image_url 字符串形态已转对象
	parts := msgs[2].(map[string]any)["content"].([]any)
	if _, ok := parts[1].(map[string]any)["image_url"].(map[string]any); !ok {
		t.Fatalf("image_url must be normalized to object form: %v", parts[1])
	}
	if obj["stream"] != true {
		t.Fatal("stream must be forced true")
	}
}

// TestPrepareBodyNoImageOrderingUntouched 无图请求的消息顺序必须完全不变。
func TestPrepareBodyNoImageOrderingUntouched(t *testing.T) {
	src := []byte(`{"model":"cn:hy3","messages":[{"role":"system","content":"s"},{"role":"user","content":"a"},{"role":"user","content":"b"}]}`)
	out := PrepareBodyOpt(src, false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if msgs[1].(map[string]any)["content"] != "a" || msgs[2].(map[string]any)["content"] != "b" {
		t.Fatalf("text-only ordering must be untouched: %v", msgs)
	}
}
