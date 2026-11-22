package server

import "testing"

func entry(id string, benefit bool) map[string]any {
	e := map[string]any{"id": id}
	if benefit {
		e["benefit"] = true
	}
	return e
}

func ids(entries []map[string]any) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		id, _ := e["id"].(string)
		out = append(out, id)
	}
	return out
}

func sameIDs(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 面板设置对 /v1/models 返回内容的控制：福利模型、隐藏指定模型。
func TestFilterModelEntries(t *testing.T) {
	entries := []map[string]any{
		entry("glm-5.2", false),
		entry("snap-benefit", true),
		entry("qwen3-vl-235b", false),
		entry("snap-chat", false),
	}
	empty := map[string]struct{}{}

	t.Run("默认全量返回", func(t *testing.T) {
		got := filterModelEntries(entries, true, empty)
		if !sameIDs(ids(got), "glm-5.2", "snap-benefit", "qwen3-vl-235b", "snap-chat") {
			t.Fatalf("got %v", ids(got))
		}
	})

	t.Run("关闭福利模型", func(t *testing.T) {
		got := filterModelEntries(entries, false, empty)
		if !sameIDs(ids(got), "glm-5.2", "qwen3-vl-235b", "snap-chat") {
			t.Fatalf("got %v", ids(got))
		}
	})

	t.Run("隐藏指定模型（大小写不敏感）", func(t *testing.T) {
		hidden := map[string]struct{}{"GLM-5.2": {}}
		got := filterModelEntries(entries, true, hidden)
		if !sameIDs(ids(got), "snap-benefit", "qwen3-vl-235b", "snap-chat") {
			t.Fatalf("got %v", ids(got))
		}
	})
}
