// 独立的 Claude Code OAuth 登录自测工具（非交互、两步式）。
//
// 因为服务器/受限终端无法交互式粘贴，本工具拆成两步，各自是一条独立命令，
// 中间用一个临时 session 文件承接 state / verifier：
//
//	第一步（生成授权 URL）：
//	    go run ./cmd/claude_login
//	  打印授权 URL 并把 session 存到临时文件。在浏览器打开该 URL 用 Claude 账号授权。
//
//	第二步（换取 token）：授权后浏览器跳到 http://localhost:54545/callback?code=...
//	  （页面打不开属正常）。直接复制**整条地址栏 URL**，或只复制 code 值，然后：
//	    go run ./cmd/claude_login -code "把整条回调URL或code粘这里"
//	  程序换取 access/refresh token、打印账号身份，并自动试刷新一次。
//
// 可选参数：
//	-proxy   出站代理，如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080
//	-session 自定义 session 文件路径（默认系统临时目录）
//	-out     把最终 token（JSON）另存到指定文件，便于后续导入账号池
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

func defaultSessionPath() string {
	return filepath.Join(os.TempDir(), "claude_login_session.json")
}

func main() {
	code := flag.String("code", "", "授权后的回调 URL 或 code 值；留空则进入第一步生成授权 URL")
	proxy := flag.String("proxy", "", "出站代理 URL（可选）")
	sessionPath := flag.String("session", defaultSessionPath(), "session 文件路径（承接 state/verifier）")
	outPath := flag.String("out", "", "可选：把最终 token JSON 另存到该文件")
	flag.Parse()

	if strings.TrimSpace(*code) == "" {
		runStart(*sessionPath)
		return
	}
	runExchange(*sessionPath, *code, *proxy, *outPath)
}

// runStart 生成授权 URL 并把 session 落盘。
func runStart(sessionPath string) {
	session, err := auth.StartClaudeLogin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "发起登录失败: %v\n", err)
		os.Exit(1)
	}
	data, _ := json.MarshalIndent(session, "", "  ")
	if err := os.WriteFile(sessionPath, data, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "写入 session 文件失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("========================================================")
	fmt.Println("第一步：在浏览器打开下面的授权 URL，用你的 Claude 账号授权")
	fmt.Println()
	fmt.Println("   " + session.AuthURL)
	fmt.Println()
	fmt.Println("授权后浏览器会跳转到 http://localhost:54545/callback?code=...（页面打不开属正常）。")
	fmt.Println("【推荐】只复制 code= 与 &state= 之间那段纯 code 值（无特殊字符，最省事）：")
	fmt.Println()
	fmt.Println("   go run ./cmd/claude_login -code '这里粘 code 值'")
	fmt.Println()
	fmt.Println("若要粘整条回调 URL，务必用【单引号】包住（否则 zsh 会把 ? & 当通配符报 no matches found）：")
	fmt.Println("   go run ./cmd/claude_login -code 'http://localhost:54545/callback?code=...&state=...'")
	fmt.Println()
	fmt.Printf("(session 已存到 %s)\n", sessionPath)
	fmt.Println("========================================================")
}

// runExchange 读取 session、换取 token 并自测刷新。
func runExchange(sessionPath, rawCode, proxy, outPath string) {
	raw, err := os.ReadFile(sessionPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取 session 文件失败(%s): %v\n请先执行第一步：go run ./cmd/claude_login\n", sessionPath, err)
		os.Exit(1)
	}
	var session auth.ClaudeLoginSession
	if err := json.Unmarshal(raw, &session); err != nil {
		fmt.Fprintf(os.Stderr, "解析 session 文件失败: %v\n", err)
		os.Exit(1)
	}

	code, stateOverride := extractCode(rawCode)
	if code == "" {
		fmt.Fprintln(os.Stderr, "未能从输入中解析出授权码。")
		os.Exit(1)
	}
	state := session.State
	if stateOverride != "" {
		state = stateOverride
	}

	client := auth.NewClaudeAuth(proxy)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Println(">> 正在换取 token ...")
	td, err := client.ExchangeCode(ctx, code, state, session.Verifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "换取 token 失败: %v\n", err)
		diagnoseClaudeLoginError(err)
		os.Exit(1)
	}
	fmt.Println(">> 登录成功！账号身份：")
	fmt.Printf("   Email        : %s\n", td.Email)
	fmt.Printf("   AccountUUID  : %s\n", td.AccountUUID)
	fmt.Printf("   Organization : %s (%s)\n", td.OrganizationName, td.OrganizationUUID)
	fmt.Printf("   AccessToken  : %s…(%d 字符)\n", safePrefix(td.AccessToken, 12), len(td.AccessToken))
	fmt.Printf("   RefreshToken : %s…(%d 字符)\n", safePrefix(td.RefreshToken, 12), len(td.RefreshToken))
	fmt.Printf("   过期时刻     : %s（约 %s 后）\n", td.ExpiresAt.Format(time.RFC3339), time.Until(td.ExpiresAt).Round(time.Second))

	if strings.TrimSpace(td.RefreshToken) != "" {
		fmt.Println("\n>> 正在用 refresh token 试刷新一次 ...")
		refreshed, rErr := client.RefreshTokens(ctx, td.RefreshToken)
		if rErr != nil {
			fmt.Fprintf(os.Stderr, "刷新失败: %v\n", rErr)
			os.Exit(1)
		}
		fmt.Printf(">> 刷新成功！新 AccessToken: %s…(%d 字符)，过期 %s\n",
			safePrefix(refreshed.AccessToken, 12), len(refreshed.AccessToken), refreshed.ExpiresAt.Format(time.RFC3339))
		td = refreshed
	} else {
		fmt.Println("\n(!) 未返回 refresh token，跳过刷新自测。")
	}

	if strings.TrimSpace(outPath) != "" {
		out := map[string]any{
			"upstream_type": auth.UpstreamClaude,
			"access_token":  td.AccessToken,
			"refresh_token": td.RefreshToken,
			"email":         td.Email,
			"account_id":    td.AccountUUID,
			"expires_at":    td.ExpiresAt.Format(time.RFC3339),
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(outPath, data, 0600); err != nil {
			fmt.Fprintf(os.Stderr, "写入 token 文件失败: %v\n", err)
		} else {
			fmt.Printf("\ntoken 已另存到 %s\n", outPath)
		}
	}
	fmt.Println("\n全链路验证通过：登录 + 身份 + 刷新均可用。")
}

// extractCode 从输入中提取授权码。支持三种形态：
//  1. 整条回调 URL：http://localhost:54545/callback?code=XXX&state=YYY
//  2. 形如 code#state 的裸串
//  3. 纯 code
//
// 返回 code 与（若能识别）state 覆盖值。
func extractCode(input string) (code, stateOverride string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		if u, err := url.Parse(input); err == nil {
			q := u.Query()
			if c := strings.TrimSpace(q.Get("code")); c != "" {
				return c, strings.TrimSpace(q.Get("state"))
			}
		}
	}
	// 裸串：交给 ExchangeCode 自行按 # 拆分 state。
	return input, ""
}

// diagnoseClaudeLoginError 按报错内容给出可能原因,便于快速定位。
func diagnoseClaudeLoginError(err error) {
	msg := strings.ToLower(err.Error())
	fmt.Fprintln(os.Stderr, "\n—— 诊断提示 ——")
	switch {
	case strings.Contains(msg, "cloudflare") || strings.Contains(msg, "just a moment") || strings.Contains(msg, "<html") || strings.Contains(msg, "attention required"):
		fmt.Fprintln(os.Stderr, "疑似被 Cloudflare 拦截。请改用 -proxy 走一个干净的住宅/机场代理重试,例如:")
		fmt.Fprintln(os.Stderr, "  go run ./cmd/claude_login -code \"<回调URL>\" -proxy http://127.0.0.1:7890")
	case strings.Contains(msg, "invalid_grant") || strings.Contains(msg, "code") && strings.Contains(msg, "expired"):
		fmt.Fprintln(os.Stderr, "授权码无效或已过期(常见:重复运行了第一步导致 session/verifier 与 code 不匹配,或 code 用过一次)。")
		fmt.Fprintln(os.Stderr, "请重新执行第一步 `go run ./cmd/claude_login` 生成新 URL,授权后立刻用新 code 执行第二步。")
	case strings.Contains(msg, "invalid_client") || strings.Contains(msg, "unauthorized_client") || strings.Contains(msg, "redirect_uri"):
		fmt.Fprintln(os.Stderr, "client_id / redirect_uri 被拒。若确认参数无误,可能是 Anthropic 侧调整,请反馈完整报错。")
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") || strings.Contains(msg, "no such host") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "tls"):
		fmt.Fprintln(os.Stderr, "网络/TLS 层失败。请检查能否直连 platform.claude.com,或加 -proxy 走代理重试。")
	default:
		fmt.Fprintln(os.Stderr, "未能自动归类。请把上面这行完整报错发给我以便定位。")
	}
	fmt.Fprintln(os.Stderr, "————————————")
}

func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
