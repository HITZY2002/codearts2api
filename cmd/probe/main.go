package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func main() {
	auths, err := auth.LoadDir("./auths")
	if err != nil || len(auths) == 0 {
		panic(fmt.Sprintf("auths: %v len=%d", err, len(auths)))
	}
	a := auths[0]
	cred := upstream.SignCredential{
		AccessKeyID: a.AccessKeyID, SecretAccessKey: a.SecretAccessKey, SecurityToken: a.CloudDragonTok,
	}
	c := upstream.New(60 * time.Second)
	msg := "hi"
	if len(os.Args) > 1 {
		msg = os.Args[1]
	}
	mode := "chatstream"
	if len(os.Args) > 2 {
		mode = os.Args[2]
	}
	model := "GLM-5.2"
	if len(os.Args) > 3 {
		model = os.Args[3]
	}
	chatID := fmt.Sprintf("%032x", time.Now().UnixNano())[:32]
	fmt.Println("chat_id=", chatID, "user=", a.UserName, "uid=", a.UserID, "msg=", msg, "mode=", mode)

	var rc io.ReadCloser
	switch mode {
	case "raw-role":
		body := map[string]any{
			"model":    model,
			"stream":   true,
			"messages": []any{map[string]any{"role": "user", "content": msg}},
		}
		rc, err = c.SendChatV2(context.Background(), body, "", cred, cred.SecurityToken, upstream.IsBenefitModel(a.UserID, model))
	case "raw-text":
		body := map[string]any{
			"model":    model,
			"stream":   true,
			"messages": []any{map[string]any{"role": "user", "content": msg}},
		}
		rc, err = c.SendChatV2(context.Background(), body, "", cred, cred.SecurityToken, upstream.IsBenefitModel(a.UserID, model))
	case "raw-blocks":
		body := map[string]any{
			"model":    model,
			"stream":   true,
			"messages": []any{map[string]any{"role": "user", "content": msg}},
		}
		rc, err = c.SendChatV2(context.Background(), body, "", cred, cred.SecurityToken, upstream.IsBenefitModel(a.UserID, model))
	default:
		rc, err = c.ChatStream(context.Background(), chatID, []upstream.ChatMessage{{Type: "text", Text: msg}}, "", cred, a.UserName, model, upstream.IsBenefitModel(a.UserID, model))
	}
	if err != nil {
		panic(err)
	}
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 64*1024)
	n := 0
	var lastText string
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			n++
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if strings.Contains(payload, `"text"`) {
					// extract text roughly
					if i := strings.Index(payload, `"text":"`); i >= 0 {
						rest := payload[i+8:]
						// naive until next unescaped "
						var b strings.Builder
						for j := 0; j < len(rest); j++ {
							if rest[j] == '\\' && j+1 < len(rest) {
								b.WriteByte(rest[j])
								b.WriteByte(rest[j+1])
								j++
								continue
							}
							if rest[j] == '"' {
								break
							}
							b.WriteByte(rest[j])
						}
						lastText = b.String()
					}
				}
			}
			if n <= 3 || strings.Contains(line, "DONE") || strings.Contains(line, "error_code") || strings.Contains(line, "error_msg") {
				if len(line) > 300 {
					fmt.Println(line[:300], "...")
				} else {
					fmt.Println(line)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
	}
	fmt.Println("total_lines=", n)
	if len(lastText) > 200 {
		fmt.Println("final_text=", lastText[:200], "...")
	} else {
		fmt.Println("final_text=", lastText)
	}
}
