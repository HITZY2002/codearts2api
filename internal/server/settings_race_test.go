package server

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// TestApplySettingsConcurrentWithReaders 覆盖「设置热更新 + 请求路径读取 +
// 面板序列化」并发场景。配合 -race 验证 h.cfg 可变字段与 WatchInfo 无数据竞争。
func TestApplySettingsConcurrentWithReaders(t *testing.T) {
	h := NewHandler(Config{
		WatchInfo: map[string]any{"enabled": true, "poll_minutes": 30},
	})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			settings := DefaultSettings()
			settings.PollMinutes = 1 + i%60
			settings.QueueRetrySeconds = 1 + i%10
			h.applySettings(settings)
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			model, delay, attempts := h.runtimeTuning()
			if model == "" || delay <= 0 || attempts <= 0 {
				t.Errorf("invalid tuning snapshot: %q %s %d", model, delay, attempts)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := json.Marshal(h.watchInfoSnapshot()); err != nil {
				t.Errorf("marshal watch info: %v", err)
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
