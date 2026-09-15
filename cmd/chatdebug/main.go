package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth dir")
	flag.Parse()
	auths, err := auth.LoadDir(*authDir)
	if err != nil { log.Fatalf("load: %v", err) }
	a := auths[0]
	cred := upstream.SignCredential{
		AccessKeyID: a.AccessKeyID, SecretAccessKey: a.SecretAccessKey, SecurityToken: a.CloudDragonTok,
	}
	c := upstream.New(60e9)
	body := map[string]any{
		"model": "GLM-5.2", "stream": true,
		"messages": []any{map[string]any{"role":"user","content":"hi"}},
	}
	rc, err := c.SendChatV2(context.Background(), body, "dbg", cred, cred.SecurityToken, upstream.IsBenefitModel(a.UserID, "GLM-5.2"))
	if err != nil { log.Fatalf("chat: %v", err) }
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 64*1024)
	n := 0
	for {
		line, err := br.ReadString('\n')
		if strings.TrimSpace(line) != "" {
			n++
			// 只打印前 8 行（看首帧结构）
			if n <= 8 {
				trimmed := strings.TrimRight(line, "\r\n")
				if len(trimmed) > 300 { trimmed = trimmed[:300]+"..." }
				fmt.Fprintf(os.Stdout, "[%d] %s\n", n, trimmed)
			}
		}
		if err == io.EOF { break }
		if err != nil { log.Fatalf("read: %v", err) }
		if n > 15 { break }
	}
	_ = json.Marshal
}
