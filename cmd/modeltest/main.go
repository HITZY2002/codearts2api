// modeltest 工具：按给定候选模型名逐个真打一次上游，输出「能不能出字」。
// 用于验证上游到底认哪个写法（显示名 vs 内部小写 ID），只读、不改账号。
//
// 用法：go run ./cmd/modeltest -auth-dir ./auths -gap 12s MODEL1 MODEL2 ...
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth dir")
	gap := flag.Duration("gap", 12*time.Second, "间隔，避免 TM.00001041 并发上限")
	flag.Parse()
	models := flag.Args()
	if len(models) == 0 {
		log.Fatal("usage: modeltest [-auth-dir ./auths] MODEL...")
	}
	auths, err := auth.LoadDir(*authDir)
	if err != nil || len(auths) == 0 {
		log.Fatalf("load auths: %v len=%d", err, len(auths))
	}
	a := auths[0]
	token, ak, sk := a.Credentials()
	c := upstream.New(90 * time.Second)
	cred := upstream.SignCredential{AccessKeyID: ak, SecretAccessKey: sk, SecurityToken: token}
	benefit := false

	for i, m := range models {
		if i > 0 {
			time.Sleep(*gap)
		}
		body := map[string]any{
			"model":    m,
			"stream":   true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		rc, err := c.SendChatV2(ctx, body, "", cred, cred.SecurityToken, benefit)
		if err != nil {
			fmt.Printf("%-28s → ERR %s\n", m, oneLine(err.Error()))
			cancel()
			continue
		}
		br := bufio.NewReaderSize(rc, 64*1024)
		var head strings.Builder
		for j := 0; j < 3; j++ {
			line, rerr := br.ReadString('\n')
			head.WriteString(line)
			if rerr != nil {
				break
			}
		}
		rc.Close()
		cancel()
		h := head.String()
		switch {
		case strings.Contains(h, "completion.chunk"):
			fmt.Printf("%-28s → OK（上游返回 %s）\n", m, upstreamModel(h))
		case strings.Contains(h, "000010051"):
			fmt.Printf("%-28s → TM.000010051 无权调用\n", m)
		case strings.Contains(h, "00001041"):
			fmt.Printf("%-28s → TM.00001041 并发上限（稍后重试）\n", m)
		default:
			fmt.Printf("%-28s → ? %s\n", m, oneLine(h))
		}
	}
}

func upstreamModel(s string) string {
	i := strings.Index(s, "\"model\":\"")
	if i < 0 {
		return "?"
	}
	rest := s[i+len("\"model\":\""):]
	if j := strings.Index(rest, "\""); j >= 0 {
		return rest[:j]
	}
	return "?"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 150 {
		s = s[:150]
	}
	return s
}
