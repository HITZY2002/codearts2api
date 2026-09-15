package server

import "testing"

// portal 的回调目标是 127.0.0.1:{port}，所以只认显式端口：
// 不写端口时保持本机端口（本机浏览器可用），写了才改写（反代场景）。
func TestPortOfCallbackHost(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"https://example.com:8443/codearts", 8443}, // 显式端口 + 路径（之前解析失败）
		{"https://example.com:443/codearts", 443},
		{"example.com:8443", 8443},                 // 无 scheme 兼容
		{"https://oneapi.suhitzy.top/codearts", 0}, // 未写端口 → 保持本机端口
		{"https://example.com", 0},
		{"", 0},
		{"not a url", 0},
	}
	for _, c := range cases {
		if got := portOfCallbackHost(c.in); got != c.want {
			t.Errorf("portOfCallbackHost(%q)=%d, want %d", c.in, got, c.want)
		}
	}
}
