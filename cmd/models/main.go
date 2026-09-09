// models 工具：打印账号当前可用模型（内置 + 限时福利），免费套餐变化时用它核对。
//
// 三路合并（与官方 ModelService 对齐）：
//  1. agent-center detail 的 gpts.models
//  2. GET /v1/model/builtin（PromptCenter）
//  3. 福利网关 GET opengw/api/v1/gateway/config（自动 claim，幂等）
//
// 用法：go run ./cmd/models [-auth-dir ./auths] [-json]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth dir")
	jsonOut := flag.Bool("json", false, "raw JSON output")
	flag.Parse()

	auths, err := auth.LoadDir(*authDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	if len(auths) == 0 {
		log.Fatalf("no accounts in %s", *authDir)
	}
	c := upstream.New(60 * time.Second)
	for _, a := range auths {
		infos, err := c.FetchModels(a)
		if err != nil {
			fmt.Printf("== %s (%s): ERROR %v\n", a.UserID, a.UserName, err)
			continue
		}
		if *jsonOut {
			raw, _ := json.MarshalIndent(map[string]any{
				"user_id":   a.UserID,
				"user_name": a.UserName,
				"models":    infos,
			}, "", "  ")
			fmt.Println(string(raw))
			continue
		}
		fmt.Printf("== %s (%s): %d models\n", a.UserID, a.UserName, len(infos))
		for _, m := range infos {
			tag := "builtin"
			if m.Benefit {
				tag = "benefit"
			}
			fmt.Printf("  [%-7s] %-28s ctx=%-8d max=%-8d %s\n",
				tag, m.ID, m.ContextWindow, m.MaxTokens, m.Desc)
		}
	}
}
