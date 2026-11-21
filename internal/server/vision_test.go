package server

import (
	"strings"
	"testing"
)

// 视觉透传：user 消息 content 数组中的 text / image_url 分片原样保留，
// 上游收到官方 image-analyze 同款 content 数组（此前 image_url 被静默丢弃，
// VL 模型"看不懂"且无提示）。
func TestVisionContentPassthrough(t *testing.T) {
	body := `{"model":"Qwen3-VL-235B","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"这是什么"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]}]}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	m := req.Messages[0]
	if len(m.ContentParts) != 2 {
		t.Fatalf("应保留 2 个分片, got %d", len(m.ContentParts))
	}
	if !strings.Contains(m.Text, "这是什么") || !strings.Contains(m.Text, "[image]") {
		t.Fatalf("Text 应含文本与占位: %q", m.Text)
	}
	msgs := buildUpstreamMessages(req, false)
	if len(msgs) != 1 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	parts, ok := msgs[0].Content.([]map[string]any)
	if !ok {
		t.Fatalf("Content 应为分片数组, got %T", msgs[0].Content)
	}
	if parts[1]["type"] != "image_url" {
		t.Fatalf("第 2 分片应为 image_url, got %v", parts[1]["type"])
	}
	iu, _ := parts[1]["image_url"].(map[string]any)
	if u, _ := iu["url"].(string); !strings.HasPrefix(u, "data:image/png") {
		t.Fatalf("url 应原样保留: %q", u)
	}
}

// 不支持的分片类型显式报错（parseChatRequest 会转成 400），不再静默丢弃。
func TestVisionUnsupportedPartRejected(t *testing.T) {
	body := `{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAA","format":"wav"}}]}]}`
	if _, err := parseChatRequest([]byte(body)); err == nil {
		t.Fatal("input_audio 分片应报错")
	}
}

// 同文本不同图片应产生不同轮次指纹（ContentParts 纳入 canonical），
// 避免不同视觉轮次共享同一 user-msg-id。
func TestVisionFingerprintDistinguishesImages(t *testing.T) {
	mk := func(url string) string {
		return `{"model":"Qwen3-VL-235B","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"` + url + `"}}]}]}`
	}
	r1, err := parseChatRequest([]byte(mk("data:image/png;base64,AAA")))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := parseChatRequest([]byte(mk("data:image/png;base64,BBB")))
	if err != nil {
		t.Fatal(err)
	}
	if upstreamTurnKey(r1) == upstreamTurnKey(r2) {
		t.Fatal("不同图片应有不同轮次指纹")
	}
}

// image_url 对象整体保留（含 detail 等 OpenAI 字段），原样透传。
func TestImageURLDetailPreserved(t *testing.T) {
	body := `{"model":"Qwen3-VL-235B","stream":false,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA","detail":"low"}}]}]}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	msgs := buildUpstreamMessages(req, false)
	parts, ok := msgs[0].Content.([]map[string]any)
	if !ok {
		t.Fatalf("Content 应为分片数组, got %T", msgs[0].Content)
	}
	iu, _ := parts[0]["image_url"].(map[string]any)
	if iu == nil || iu["detail"] != "low" {
		t.Fatalf("detail 应原样保留, got %v", iu)
	}
}

// 纯文本消息行为不变：无 ContentParts，Content 仍为字符串。
func TestTextOnlyMessageUnchanged(t *testing.T) {
	body := `{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"你好"}]}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].ContentParts != nil {
		t.Fatal("纯文本不应产生分片")
	}
	msgs := buildUpstreamMessages(req, false)
	if _, ok := msgs[0].Content.(string); !ok {
		t.Fatalf("纯文本 Content 应为 string, got %T", msgs[0].Content)
	}
}
