// login 工具：华为云 CodeArts OAuth2（PKCE）登录 → 保存 auths/codearts-{user_id}.json。
//
// 流程（对齐 huaweicloud.authentication 扩展）：
//  1. 本地起 127.0.0.1 回调服务
//  2. 生成 ticket_id/secret + PKCE，构造 codearts.huaweicloud.com/authorize 链接
//  3. 浏览器登录 → portal 回调本地服务两次（先 secret+redirect，再 code），
//     或轮询 snap-manager /v1/login/ticket（用 portal 下发的 secret）
//  4. oauth2/tokens 换 STS 临时 AK/SK + security_token + refresh_token → 落盘
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth output dir")
	printOnly := flag.Bool("print-only", false, "server mode: print login link and poll (no local browser)")
	clientID := flag.String("client-id", upstream.CLIENT_ID, "OAuth client id (uri scheme)")
	flag.Parse()

	cfg := upstream.DefaultLoginConfig()
	if *clientID != "" {
		cfg.ClientID = *clientID
	}
	client := upstream.New(60 * time.Second)
	ctx := context.Background()

	// 1. 本地回调服务
	var ln net.Listener
	var err error
	callbackPort := 0
	codeCh := make(chan string, 1)
	cb := &callbackState{}
	if !*printOnly {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("loopback listen: %v", err)
		}
		callbackPort = ln.Addr().(*net.TCPAddr).Port
		go serveCallback(ln, cfg.RedirectPath, cb, codeCh)
		defer ln.Close()
	}

	// 2. 生成 ticket/secret + PKCE
	ticketID, err := upstream.RandomHex(16)
	if err != nil {
		log.Fatalf("gen ticket: %v", err)
	}
	secret, err := upstream.RandomHex(16)
	if err != nil {
		log.Fatalf("gen secret: %v", err)
	}
	verifier, challenge, err := upstream.PKCE()
	if err != nil {
		log.Fatalf("gen pkce: %v", err)
	}
	// DPoP 私钥必须与 refresh_token 一起持久化：refresh_token 与签发时的 DPoP
	// 公钥绑定，换密钥刷新会被 STS 拒（invalid refresh token: InvalidDPoPHeader）。
	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		log.Fatalf("gen dpop: %v", err)
	}

	loginURL := client.BuildAuthorizeURL(cfg, ticketID, challenge, "SHA-256", callbackPort)
	fmt.Println("============================================================")
	fmt.Println("  CodeArts Agent 登录（华为云账号）")
	fmt.Println("============================================================")
	fmt.Println("步骤：")
	fmt.Println("  1. 打开下面链接，用华为云账号完成登录")
	fmt.Println("  2. 登录成功后浏览器会跳回 127.0.0.1（服务器模式跳转失败可忽略）")
	fmt.Println("  3. 本工具自动换取 STS 临时凭证并落盘 auths/")
	fmt.Println("")
	fmt.Println("登录链接：")
	fmt.Println("  " + loginURL)
	fmt.Println("")

	if !*printOnly {
		_ = openBrowser(loginURL)
	}

	// 3. 双通道：回调 code vs ticket 轮询
	var tok *upstream.TokenResponse
	ticker := time.NewTicker(2 * time.Second)
	timer := time.NewTimer(5 * time.Minute)
	defer ticker.Stop()
	defer timer.Stop()
loginLoop:
	for {
		select {
		case <-timer.C:
			log.Fatalf("登录超时（5 分钟）")
		case code := <-codeCh:
			tok, err = client.ExchangeCode(ctx, cfg, code, verifier, callbackPort, dpopPrivateJWK)
			if err != nil {
				log.Fatalf("exchange code: %v", err)
			}
			break loginLoop
		case <-ticker.C:
			t, terr := client.PollTicket(ctx, cfg, ticketID, cb.pollSecret(secret))
			if terr == nil && t != nil && t.UserName != "" {
				tok = t
				break loginLoop
			}
		}
	}

	// 4. 落盘
	cred := tok.Credentials
	a := auth.New(tok.UserID, tok.UserName, tok.DomainID,
		cred.SecurityToken, cred.AccessKeyID, cred.SecretAccessKey,
		cred.Expiration, tok.RefreshToken, verifier)
	a.SetClientID(cfg.ClientID)
	a.SetDPoPPrivateKey(dpopPrivateJWK)
	if err := auth.SaveNew(*authDir, a); err != nil {
		log.Fatalf("save auth: %v", err)
	}
	fmt.Printf("\n✅ 登录成功：user_id=%s name=%s\n", tok.UserID, tok.UserName)
	fmt.Printf("凭证已保存：%s\n", filepath.Join(*authDir, a.FileName()))
	if cred.Expiration != "" {
		fmt.Printf("STS 有效期至：%s（到期前自动 refresh 续期）\n", cred.Expiration)
	}
}

// callbackState 保存 portal 回调里下发的 secret。
//
// portal 首次回调只带 secret + redirect（不含 code），而轮询 /v1/login/ticket 必须用
// portal 下发的 secret（本地生成的那份上游不认），所以两边都要用回调里收到的值。
type callbackState struct {
	mu     sync.Mutex
	secret string
}

func (s *callbackState) setSecret(v string) {
	s.mu.Lock()
	s.secret = v
	s.mu.Unlock()
}

// pollSecret 返回 portal 下发的 secret；portal 没下发时退回本地生成的。
func (s *callbackState) pollSecret(fallback string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret != "" {
		return s.secret
	}
	return fallback
}

// serveCallback 处理本地回调（GET query 或 POST body 传 code）。
//
// portal 会回调两次：第一次带 secret + redirect，要求 307 跳回 redirect（登录页链路
// 的一环）；在这一次报错会中断链路，带 code 的第二次回调永远不会到达。第二次带 code。
func serveCallback(ln net.Listener, redirectPath string, st *callbackState, ch chan<- string) {
	mux := http.NewServeMux()
	mux.HandleFunc(redirectPath, func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		secret := r.URL.Query().Get("secret")
		redirect := r.URL.Query().Get("redirect")
		if secret != "" {
			st.setSecret(secret)
		}
		if code == "" && redirect != "" {
			http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
			return
		}
		if code == "" {
			// 兼容 POST body
			body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			vals, _ := url.ParseQuery(string(body))
			code = vals.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<h3>登录失败：缺少 code</h3>"))
			return
		}
		_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面。</h3>"))
		select {
		case ch <- code:
		default:
		}
	})
	_ = http.Serve(ln, mux)
}

func openBrowser(rawurl string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawurl)
	case "darwin":
		cmd = exec.Command("open", rawurl)
	default:
		cmd = exec.Command("xdg-open", rawurl)
	}
	return cmd.Start()
}
