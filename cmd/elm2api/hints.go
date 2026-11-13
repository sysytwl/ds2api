package main

import (
	"log"
	"strings"
	"sync"
	"time"
)

// cookieHelp is printed in the shell and embedded in API errors whenever the
// ELM SESSION cookie is missing or expired. It embeds the real listen address
// so the copy-paste curl command always targets the running service.
func cookieHelp(listen string) string {
	url := "http://" + displayListen(listen)
	return "获取/更新 ELM SESSION cookie：" +
		"浏览器登录 https://elm.edina.ac.uk/elm → F12 → Application(应用) → Cookies → elm.edina.ac.uk → 复制 SESSION 的值；" +
		"然后二选一：(a) 写进 config.json 的 sessions[].cookie（或设置环境变量 ELM_SESSION）后重启服务；" +
		"(b) 不重启：curl -X POST " + url + "/api/sessions -H 'Content-Type: application/json' -d '{\"name\":\"main\",\"cookie\":\"SESSION=...\"}'"
}

// displayListen turns a bind address into a URL host that a local browser can
// actually open.
func displayListen(listen string) string {
	addr := strings.TrimSpace(listen)
	if addr == "" {
		return "127.0.0.1:8787"
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	if strings.HasPrefix(addr, "[::]:") {
		return "127.0.0.1:" + strings.TrimPrefix(addr, "[::]:")
	}
	return addr
}

var (
	expiryLogMu sync.Mutex
	expiryLogAt = map[string]time.Time{}
)

// logExpired prints the actionable shell message when a session dies. It is
// throttled per session so a busy client cannot flood the terminal.
func logExpired(sessionName, email, listen string) {
	key := sessionName + "|" + email
	expiryLogMu.Lock()
	last := expiryLogAt[key]
	now := time.Now()
	expiryLogAt[key] = now
	expiryLogMu.Unlock()
	if !last.IsZero() && now.Sub(last) < 30*time.Second {
		return
	}
	who := strings.TrimSpace(email)
	if who == "" {
		who = strings.TrimSpace(sessionName)
	}
	log.Printf("elm2api: ⚠ ELM SESSION cookie 已过期/失效（会话 %q，%s）", sessionName, who)
	log.Printf("elm2api: %s", cookieHelp(listen))
	log.Printf("elm2api: 服务仍在 http://%s 运行，修复 cookie 后无需重启（方式 b）", displayListen(listen))
}

func logRecovered(sessionName, email string) {
	log.Printf("elm2api: ✔ ELM SESSION 已恢复（会话 %q，%s）", sessionName, email)
}

func logNoSession(listen, baseURL string) {
	log.Printf("elm2api: ⚠ 当前没有可用的 ELM 会话，服务先启动在 http://%s（上游 %s）", displayListen(listen), baseURL)
	log.Printf("elm2api: %s", cookieHelp(listen))
}
