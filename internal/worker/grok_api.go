package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/grpcweb"
	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/tempmail"
	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/turnstile"
)

// grokRegRequest 远程 Grok 注册请求（HF 端接收）
type grokRegRequest struct {
	Proxy                string `json:"proxy"`
	YYDSMailURL          string `json:"yydsmail_url"`
	YYDSMailKey          string `json:"yydsmail_key"`
	EmailPriority        string `json:"email_priority"`
	TurnstileSolverURL   string `json:"turnstile_solver_url"`
	TurnstileSolverProxy string `json:"turnstile_solver_proxy"`
	CFBypassSolverURL    string `json:"cf_bypass_solver_url"`
	CapSolverKey         string `json:"capsolver_key"`
	YesCaptchaKey        string `json:"yescaptcha_key"`
	SiteKey              string `json:"site_key"`
	ActionID             string `json:"action_id"`
	StateTree            string `json:"state_tree"`
}

// GrokProtocolRegisterHandler 完整 Grok 注册端点（HF Space 独立运行）
// POST /api/v1/process
// 供远程调用：日本后端 → CF Workers → HF Space
// 流程: 创建邮箱 → gRPC-web 发/收验证码 → Turnstile → Server Action → SSO → NSFW
func GrokProtocolRegisterHandler(c *gin.Context) {
	var req grokRegRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "invalid request: " + err.Error()})
		return
	}

	// 构建 multi-provider 邮箱配置
	priority := req.EmailPriority
	if priority == "" {
		priority = "yydsmail"
	}
	cfg := map[string]string{
		"yydsmail_api_key":        req.YYDSMailKey,
		"yydsmail_base_url":       req.YYDSMailURL,
		"email_provider_priority": priority,
	}
	mailProvider := tempmail.NewMultiProvider(cfg)

	// 日志收集
	logs := make([]string, 0, 30)
	logf := func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		logs = append(logs, msg)
		fmt.Printf("[LOG] %s\n", msg) // 同时打印到 stdout，方便调试
	}

	// 诊断日志：记录收到的配置
	logf("[*] 邮箱优先级: %s", priority)
	creds := ""
	if req.YYDSMailKey != "" {
		creds += "yydsmail "
	}
	if creds == "" {
		creds = "(未配置凭证)"
	}
	logf("[*] 可用凭证: %s", creds)
	if req.Proxy != "" {
		logf("[*] 代理: %s", req.Proxy)
	}

	// 排队等待信息（由 ConcurrencyLimiter 中间件注入 gin context）
	if waited, ok := c.Get("queue_waited"); ok && waited.(bool) {
		pos, _ := c.Get("queue_position")
		waitSec, _ := c.Get("queue_wait_seconds")
		logf("[*] 排队等待完成: 位置 #%d, 等待 %d 秒", pos, waitSec)
	}

	// 构建代理
	var proxy *ProxyEntry
	if req.Proxy != "" {
		proxy = &ProxyEntry{HTTPS: req.Proxy, HTTP: req.Proxy}
	}

	// 执行上下文（5 分钟超时，覆盖 Turnstile + 验证码轮询 + 注册 + NSFW）
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()

	w := &GrokWorker{}
	profile := browserProfiles[rand.Intn(len(browserProfiles))]
	logf("[*] 任务开始")

	// ─── 创建 HTTP 会话 ───
	client := w.makeHTTPClient(proxy, 30*time.Second)

	// 会话预热（获取 __cf_bm cookie）
	warmupReq, _ := http.NewRequestWithContext(ctx, "GET", "https://accounts.x.ai", nil)
	warmupReq.Header.Set("User-Agent", profile.UserAgent)
	if resp, err := client.Do(warmupReq); err == nil {
		resp.Body.Close()
	}

	// ─── Phase 0: 配置扫描 + Solver 初始化（在邮箱创建之前，避免浪费邮箱） ───

	// Turnstile solver 配置（优先请求参数，Mac Mini 环境下回退到容器环境变量）
	var solverURLs []string
	if req.TurnstileSolverURL != "" {
		solverURLs = append(solverURLs, req.TurnstileSolverURL)
	} else if envURL := os.Getenv("TURNSTILE_SOLVER_URL"); envURL != "" {
		solverURLs = append(solverURLs, envURL)
		logf("[*] Turnstile solver: %s (环境变量)", envURL)
	}
	if req.CFBypassSolverURL != "" {
		solverURLs = append(solverURLs, req.CFBypassSolverURL)
	} else if envURL := os.Getenv("CF_BYPASS_SOLVER_URL"); envURL != "" {
		solverURLs = append(solverURLs, envURL)
		logf("[*] CF bypass solver: %s (环境变量)", envURL)
	}
	// Solver 专用代理：优先用面板配的 turnstile_solver_proxy，没有才用注册代理
	var solverProxyURL string
	if req.TurnstileSolverProxy != "" {
		solverProxyURL = req.TurnstileSolverProxy
	} else if proxy != nil {
		if proxy.HTTPS != "" {
			solverProxyURL = proxy.HTTPS
		} else if proxy.HTTP != "" {
			solverProxyURL = proxy.HTTP
		}
	}
	solver := turnstile.NewSolver(solverURLs, req.CapSolverKey, req.YesCaptchaKey, solverProxyURL)

	siteKey := req.SiteKey
	if siteKey == "" {
		siteKey = "0x4AAAAAAAhr9JGVDZbrZOo0" // 默认 fallback
	}
	actionID := req.ActionID
	stateTree := req.StateTree
	if stateTree == "" {
		stateTree = defaultStateTree
	}

	// action_id 为空时自行扫描（HF Space 可直连 x.ai）
	if actionID == "" {
		logf("[*] 注册配置未提供，自行扫描获取...")
		scanner := &GrokWorker{}
		scanned, scanErr := scanner.doScan(ctx, proxy)
		if scanErr == nil {
			if aid := scanned["action_id"]; aid != "" {
				actionID = aid
				logf("[+] 注册配置已获取")
			}
			if sk := scanned["site_key"]; sk != "" {
				siteKey = sk
			}
			if st := scanned["state_tree"]; st != "" {
				stateTree = st
			}
		} else {
			logf("[-] 自行扫描失败: %s", scanErr)
		}
	}

	if actionID == "" {
		logf("[-] 缺少注册配置且扫描失败，无法注册")
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": "missing configuration", "logs": logs})
		return
	}

	// ─── Phase 1+2: 创建临时邮箱 + 发送验证码（带域名黑名单 + 拒绝检测重试） ───
	var email string
	var mailMeta map[string]string
	password := randomString(15, "abcdefghijklmnopqrstuvwxyz0123456789")

	emailOK := false
	for emailAttempt := 0; emailAttempt < 5; emailAttempt++ {
		if ctx.Err() != nil {
			break
		}

		// Phase 1: 创建临时邮箱
		candidate, meta, err := mailProvider.GenerateEmail(ctx)
		if err != nil {
			logf("[-] 创建邮箱失败: %s", err)
			continue
		}

		// 检查域名黑名单
		domain := emailDomain(candidate)
		if grokIsDomainBanned(domain) {
			logf("[*] 跳过已封禁域名 %s，重新申请邮箱...", domain)
			go mailProvider.DeleteEmail(context.Background(), candidate, meta)
			continue
		}

		email = candidate
		mailMeta = meta
		logf("[*] 邮箱: %s", email)

		// Phase 2: 发送验证码 (gRPC-web)
		logf("[*] 发送验证码...")
		grpcBody := grpcweb.EncodeEmailCode(email)
		sendResp, sendErr := w.doGRPCWeb(ctx, client, profile.UserAgent,
			"https://accounts.x.ai/auth_mgmt.AuthManagement/CreateEmailValidationCode",
			"https://accounts.x.ai", "https://accounts.x.ai/sign-up?redirect=grok-com",
			grpcBody, nil)
		if sendErr != nil || sendResp == nil || sendResp.StatusCode != 200 {
			status := 0
			var respBody string
			if sendResp != nil {
				status = sendResp.StatusCode
				bodyBytes, _ := io.ReadAll(sendResp.Body)
				sendResp.Body.Close()
				respBody = string(bodyBytes)
			}
			// 检测域名拒绝
			if strings.Contains(strings.ToLower(respBody), "rejected") || strings.Contains(strings.ToLower(respBody), "banned") || strings.Contains(strings.ToLower(respBody), "blocked") {
				logf("[*] 邮箱域名 %s 被拒绝，加入黑名单并重试...", domain)
				grokBanDomain(domain)
				go mailProvider.DeleteEmail(context.Background(), email, mailMeta)
				continue
			}
			logf("[-] 发送验证码失败: HTTP %d, %v", status, sendErr)
			go mailProvider.DeleteEmail(context.Background(), email, mailMeta)
			c.JSON(http.StatusOK, gin.H{"ok": false, "error": "verification failed", "email": email, "logs": logs})
			return
		}
		sendResp.Body.Close()
		logf("[+] 验证码已发送")
		emailOK = true
		break
	}
	if !emailOK {
		logf("[-] 多次尝试后仍无法创建可用邮箱")
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": "email creation failed after retries", "logs": logs})
		return
	}

	// 注册失败时清理邮箱
	defer func() {
		go mailProvider.DeleteEmail(context.Background(), email, mailMeta)
	}()

	// ─── Phase 3+4: 获取验证码 + 验证邮箱（带重试和排除已试过的验证码） ───
	triedCodes := make(map[string]bool)
	verified := false
	var verifiedCode string

	for codeAttempt := 0; codeAttempt < 3; codeAttempt++ {
		if ctx.Err() != nil {
			break
		}
		if codeAttempt > 0 {
			// 重新发送验证码
			logf("[*] 重新发送验证码（第 %d 次重试）...", codeAttempt)
			grpcBody := grpcweb.EncodeEmailCode(email)
			sendResp, sendErr := w.doGRPCWeb(ctx, client, profile.UserAgent,
				"https://accounts.x.ai/auth_mgmt.AuthManagement/CreateEmailValidationCode",
				"https://accounts.x.ai", "https://accounts.x.ai/sign-up?redirect=grok-com",
				grpcBody, nil)
			if sendErr == nil && sendResp != nil {
				sendResp.Body.Close()
			}
			ctxSleep(ctx, 2*time.Second)
		}

		// Phase 3: 获取验证码
		logf("[*] 等待验证码（最长 45s）...")
		var code string
		for attempt := 1; attempt <= 45; attempt++ {
			select {
			case <-ctx.Done():
				logf("[-] 等待验证码时任务被取消")
				c.JSON(http.StatusOK, gin.H{"ok": false, "error": "cancelled", "email": email, "logs": logs})
				return
			case <-time.After(1 * time.Second):
			}
			c2, fetchErr := mailProvider.FetchVerificationCode(ctx, email, mailMeta, 1, 0)
			if fetchErr == nil && c2 != "" && !triedCodes[c2] {
				code = c2
				break
			}
			if attempt%5 == 0 {
				logf("[*] 等待验证码中... 已等待 %ds", attempt)
			}
		}
		if code == "" {
			logf("[-] 获取验证码超时")
			if codeAttempt == 0 {
				tempmail.RecordFailure(mailMeta["provider"])
			}
			continue
		}
		logf("[+] 验证码: %s", code)
		triedCodes[code] = true
		if codeAttempt == 0 {
			tempmail.RecordSuccess(mailMeta["provider"])
		}

		// Phase 4: 验证邮箱 (gRPC-web)
		verifyBody := grpcweb.EncodeVerifyCode(email, code)
		verifyResp, verifyErr := w.doGRPCWeb(ctx, client, profile.UserAgent,
			"https://accounts.x.ai/auth_mgmt.AuthManagement/VerifyEmailValidationCode",
			"https://accounts.x.ai", "https://accounts.x.ai/sign-up?redirect=grok-com",
			verifyBody, nil)
		if verifyErr != nil || verifyResp == nil || verifyResp.StatusCode != 200 {
			if verifyResp != nil {
				verifyResp.Body.Close()
			}
			logf("[-] 验证码 %s 验证失败，排除后重试...", code)
			continue
		}
		verifyResp.Body.Close()
		logf("[+] 邮箱验证成功")
		verified = true
		verifiedCode = code
		break
	}
	if !verified {
		logf("[-] 验证码验证最终失败")
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": "validation failed after retries", "email": email, "logs": logs})
		return
	}

	// ─── Phase 5+6: Turnstile + Server Actions 注册 ───
	firstName := randomName(4, 6)
	lastName := randomName(4, 6)

	var ssoToken string
	registered := false

	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-ctx.Done():
			c.JSON(http.StatusOK, gin.H{"ok": false, "error": "timeout", "email": email, "logs": logs})
			return
		default:
		}

		logf("[*] 注册尝试 %d/3...", attempt+1)

		// Turnstile 求解
		logf("[*] 验证远程服务接口...")
		turnstileToken, err := solver.Solve(ctx, "https://accounts.x.ai/sign-up", siteKey, 3, logf)
		if err != nil {
			logf("[-] 验证失败: %s", err)
			ctxSleep(ctx, 3*time.Second)
			continue
		}
		logf("[+] 卧槽Σ(°ロ°)验证通过 (%d chars)", len(turnstileToken))

		// 用同一个 Turnstile token 提交注册，404 时重新扫描 action_id 并立即重试（最多 2 次）
		var respText string
		regOK := false
		for regRetry := 0; regRetry < 2; regRetry++ {
			regPayload := []map[string]interface{}{{
				"emailValidationCode": verifiedCode,
				"createUserAndSessionRequest": map[string]interface{}{
					"email":              email,
					"givenName":          firstName,
					"familyName":         lastName,
					"clearTextPassword":  password,
					"tosAcceptedVersion": "$undefined",
				},
				"turnstileToken":         turnstileToken,
				"promptOnDuplicateEmail": true,
			}}
			jsonBody, _ := json.Marshal(regPayload)

			regReq, _ := http.NewRequestWithContext(ctx, "POST", "https://accounts.x.ai/sign-up", bytes.NewReader(jsonBody))
			regReq.Header.Set("Content-Type", "text/plain;charset=UTF-8")
			regReq.Header.Set("Accept", "text/x-component")
			regReq.Header.Set("Next-Action", actionID)
			regReq.Header.Set("Next-Router-State-Tree", stateTree)
			regReq.Header.Set("Origin", "https://accounts.x.ai")
			regReq.Header.Set("Referer", "https://accounts.x.ai/sign-up")
			regReq.Header.Set("User-Agent", profile.UserAgent)

			regResp, err := client.Do(regReq)
			if err != nil {
				logf("[-] 注册请求失败: %s", err)
				break
			}
			respBody, _ := io.ReadAll(regResp.Body)
			regResp.Body.Close()
			respText = string(respBody)

			if regResp.StatusCode == 200 {
				regOK = true
				break
			}

			logf("[-] 注册响应: HTTP %d", regResp.StatusCode)
			// action_id 过期 → 重新扫描刷新，用同一个 token 立即重试
			if regResp.StatusCode == 404 && strings.Contains(respText, "action not found") {
				logf("[*] 注册配置已过期，重新扫描...")
				scanner := &GrokWorker{}
				if scanned, scanErr := scanner.doScan(ctx, proxy); scanErr == nil {
					if aid := scanned["action_id"]; aid != "" {
						actionID = aid
						logf("[+] 注册配置已刷新，用同一个 token 立即重试")
					}
					if st := scanned["state_tree"]; st != "" {
						stateTree = st
					}
					ctxSleep(ctx, 1*time.Second)
					continue // 内层循环重试，不重新求解 Turnstile
				} else {
					logf("[-] 重新扫描失败: %s", scanErr)
				}
			}
			break // 非 404 错误直接退出内层循环
		}
		if !regOK {
			ctxSleep(ctx, 3*time.Second)
			continue
		}

		// 提取 set-cookie URL
		var cookieURLMatch string
		if sm := reCookieURLAnchored.FindStringSubmatch(respText); len(sm) > 1 {
			cookieURLMatch = sm[1]
		} else if sm := reCookieURLQuoted.FindStringSubmatch(respText); len(sm) > 1 {
			cookieURLMatch = sm[1]
		}
		if cookieURLMatch == "" {
			logf("[-] 会话处理失败，响应异常")
			ctxSleep(ctx, 3*time.Second)
			continue
		}

		// 请求 set-cookie URL 获取 SSO token
		cookieReq, _ := http.NewRequestWithContext(ctx, "GET", cookieURLMatch, nil)
		cookieReq.Header.Set("User-Agent", profile.UserAgent)
		cookieResp, err := client.Do(cookieReq)
		if err != nil {
			logf("[-] 会话回调失败: %s", err)
			break
		}

		for _, sc := range cookieResp.Header["Set-Cookie"] {
			if strings.HasPrefix(sc, "sso=") && !strings.HasPrefix(sc, "sso-rw=") {
				parts := strings.SplitN(sc, ";", 2)
				kv := strings.SplitN(parts[0], "=", 2)
				if len(kv) == 2 && kv[1] != "" {
					ssoToken = kv[1]
				}
			}
		}
		cookieResp.Body.Close()

		// 降级: cookie jar 里找 sso
		if ssoToken == "" {
			for _, domain := range []string{
				cookieResp.Request.URL.String(),
				"https://accounts.x.ai",
				"https://grok.com",
				"https://x.ai",
			} {
				domainURL, err := url.Parse(domain)
				if err != nil {
					continue
				}
				for _, ck := range client.Jar.Cookies(domainURL) {
					if ck.Name == "sso" && ssoToken == "" {
						ssoToken = ck.Value
					}
				}
			}
		}

		if ssoToken == "" {
			logf("[-] 未获取到登录凭证")
			break
		}

		logf("[+] 注册成功!")
		registered = true
		break
	}

	if !registered {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": "process failed", "email": email, "logs": logs})
		return
	}

	// ─── Phase 7+8: TOS + 生日 + NSFW + Unhinged + PayUrl ───
	// HF 上只用 enableNSFWGo（纯 Go），不带 Python fallback
	logf("[*] 马上处理..设置中...")
	nsfwEnabled, checkoutURL := w.enableNSFWGo(ctx, ssoToken, email, proxy, logf)
	if nsfwEnabled {
		logf("[+] 马上处理..成功啦")
	} else {
		logf("[!] 马上处理..失败（账号已创建，继续返回）")
	}

	logf("[OK] 任务完成: %s", email)

	// 注入邮箱元数据，供后续 OTP 查询使用
	emailMetaCopy := make(map[string]interface{}, len(mailMeta))
	for k, v := range mailMeta {
		emailMetaCopy[k] = v
	}
	result := gin.H{
		"ok":              true,
		"email":           email,
		"auth_token":      ssoToken,
		"feature_enabled": nsfwEnabled,
		"provider":        mailMeta["provider"],
		"email_meta":      emailMetaCopy,
		"logs":            logs,
	}
	if checkoutURL != "" {
		result["redirect_url"] = checkoutURL
	}
	c.JSON(http.StatusOK, result)
}
