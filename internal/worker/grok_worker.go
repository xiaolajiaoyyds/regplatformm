package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	cryptorand "crypto/rand"
	"math/big"

	"github.com/rs/zerolog/log"
	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/grpcweb"
	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/tempmail"
	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/turnstile"
)

// ─── TLS 指纹模拟配置 ───

// browserProfile 浏览器指纹配置
type browserProfile struct {
	Name      string
	UserAgent string
}

var browserProfiles = []browserProfile{
	{Name: "chrome120", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"},
	{Name: "chrome119", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"},
	{Name: "chrome110", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/110.0.0.0 Safari/537.36"},
	{Name: "edge99", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/99.0.4844.74 Safari/537.36 Edg/99.0.1150.46"},
	{Name: "edge101", UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/101.0.4951.64 Safari/537.36 Edg/101.0.1210.53"},
}

// ─── 常量 ───

// defaultStateTree Next.js 路由状态树默认值（sign-up 页面）
const defaultStateTree = `%5B%22%22%2C%7B%22children%22%3A%5B%22(app)%22%2C%7B%22children%22%3A%5B%22(auth)%22%2C%7B%22children%22%3A%5B%22sign-up%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2C%22%2Fsign-up%22%2C%22refresh%22%5D%7D%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D`

// ─── 正则表达式 ───

var (
	reSiteKey   = regexp.MustCompile(`sitekey":"(0x4[a-zA-Z0-9_-]+)"`)
	reStateTree = regexp.MustCompile(`next-router-state-tree":"([^"]+)"`)
	reScriptSrc = regexp.MustCompile(`<script[^>]+src="([^"]*_next/static[^"]*)"`)
	reActionID  = regexp.MustCompile(`7f[a-fA-F0-9]{40}`)
	// 参考 grokzhuce: 用 \d+: 锚定 RSC 段号分隔符，防止 URL 末尾吃掉多余数字
	reCookieURLAnchored = regexp.MustCompile(`(https://[^"\s]+set-cookie\?q=[^:"\s]+)\d+:`)
	// 降级: JSON 引号内的 URL（引号自然截止）
	reCookieURLQuoted = regexp.MustCompile(`(https://[^"\s]+set-cookie\?q=[^:"\s]+)`)
)

// ─── 邮箱域名黑名单（进程级缓存） ───

var (
	grokBannedDomains   = make(map[string]time.Time) // domain → banned time
	grokBannedDomainsMu sync.RWMutex
)

// grokBanDomain 将域名加入黑名单
func grokBanDomain(domain string) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return
	}
	grokBannedDomainsMu.Lock()
	grokBannedDomains[domain] = time.Now()
	grokBannedDomainsMu.Unlock()
}

// grokIsDomainBanned 检查域名是否在黑名单中（1小时过期自动清除）
func grokIsDomainBanned(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return false
	}
	grokBannedDomainsMu.RLock()
	bannedAt, ok := grokBannedDomains[domain]
	grokBannedDomainsMu.RUnlock()
	if !ok {
		return false
	}
	// 1小时过期，避免永久封禁误判
	if time.Since(bannedAt) > time.Hour {
		grokBannedDomainsMu.Lock()
		delete(grokBannedDomains, domain)
		grokBannedDomainsMu.Unlock()
		return false
	}
	return true
}

// emailDomain 提取邮箱的域名部分
func emailDomain(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.ToLower(parts[1])
}

// ─── GrokWorker 实现 ───

// GrokWorker Grok 平台注册器（完整实现）
type GrokWorker struct{}

func init() {
	Register(&GrokWorker{})
}

func (w *GrokWorker) PlatformName() string { return "grok" }

// ScanConfig 扫描 Grok 注册页面获取 site_key、action_id、state_tree
// Server Actions 注册模式必须获取 action_id
func (w *GrokWorker) ScanConfig(ctx context.Context, proxy *ProxyEntry, cfg Config) (Config, error) {
	// 优先读 DB 已持久化的值作为 fallback，避免 X.AI 更新 site_key 后硬编码过期
	fallbackSiteKey := cfg["grok_site_key"]
	if fallbackSiteKey == "" {
		fallbackSiteKey = "0x4AAAAAAAhr9JGVDZbrZOo0"
	}
	result := Config{
		"site_key":   fallbackSiteKey,
		"state_tree": defaultStateTree,
	}

	// 远程服务模式：VPS 无需直连 x.ai，HF worker 会自行获取 action_id
	if cfg["grok_reg_url"] != "" {
		// 尝试用 DB 里缓存的 action_id，没有也不报错
		if aid := cfg["grok_action_id"]; aid != "" {
			result["action_id"] = aid
		}
		log.Info().Msg("remote mode, skip local scan")
		return result, nil
	}

	// 1) 直连扫描
	proxies := []*ProxyEntry{proxy}
	if proxy != nil {
		proxies = append(proxies, nil)
	}
	for _, p := range proxies {
		scanned, err := w.doScan(ctx, p)
		if err == nil {
			for _, k := range []string{"site_key", "action_id", "state_tree"} {
				if v := scanned[k]; v != "" {
					result[k] = v
				}
			}
			if result["action_id"] != "" {
				log.Info().Msg("config scan ok")
				return result, nil
			}
		}
	}

	// 2) CF-Bypass 降级扫描
	if cfBypassURL := cfg["cf_bypass_solver_url"]; cfBypassURL != "" {
		scanned, err := w.doScanViaCFBypass(ctx, cfBypassURL)
		if err == nil {
			for _, k := range []string{"site_key", "action_id", "state_tree"} {
				if v := scanned[k]; v != "" {
					result[k] = v
				}
			}
			if result["action_id"] != "" {
				log.Info().Msg("bypass scan ok")
				return result, nil
			}
		}
	}

	if result["action_id"] == "" {
		return result, fmt.Errorf("扫描失败: 未获取到 action_id（Server Actions 注册需要此配置）")
	}
	return result, nil
}

// doScan 实际扫描逻辑
func (w *GrokWorker) doScan(ctx context.Context, proxy *ProxyEntry) (Config, error) {
	cfg := Config{
		"site_key":   "0x4AAAAAAAhr9JGVDZbrZOo0",
		"action_id":  "",
		"state_tree": "%5B%22%22%2C%7B%22children%22%3A%5B%22(app)%22%2C%7B%22children%22%3A%5B%22(auth)%22%2C%7B%22children%22%3A%5B%22sign-up%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2C%22%2Fsign-up%22%2C%22refresh%22%5D%7D%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D",
	}

	client := w.makeHTTPClient(proxy, 30*time.Second)
	profile := browserProfiles[rand.Intn(len(browserProfiles))]

	// GET 注册页面
	req, err := http.NewRequestWithContext(ctx, "GET", "https://accounts.x.ai/sign-up", nil)
	if err != nil {
		return cfg, err
	}
	req.Header.Set("User-Agent", profile.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return cfg, fmt.Errorf("扫描注册页失败: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // 1MB 上限（HTML 页面）
	resp.Body.Close()
	html := string(body)

	// 提取 site_key
	if m := reSiteKey.FindStringSubmatch(html); len(m) > 1 {
		cfg["site_key"] = m[1]
	}

	// 提取 state_tree
	if m := reStateTree.FindStringSubmatch(html); len(m) > 1 {
		cfg["state_tree"] = m[1]
	}

	// 提取 JS bundle URL，搜索 action_id
	scriptMatches := reScriptSrc.FindAllStringSubmatch(html, -1)
	for _, sm := range scriptMatches {
		if len(sm) < 2 {
			continue
		}
		jsURL := sm[1]
		if !strings.HasPrefix(jsURL, "http") {
			jsURL = "https://accounts.x.ai" + jsURL
		}

		jsReq, _ := http.NewRequestWithContext(ctx, "GET", jsURL, nil)
		jsReq.Header.Set("User-Agent", profile.UserAgent)
		jsResp, err := client.Do(jsReq)
		if err != nil {
			continue
		}
		jsBody, _ := io.ReadAll(jsResp.Body)
		jsResp.Body.Close()

		if m := reActionID.FindString(string(jsBody)); m != "" {
			cfg["action_id"] = m
			break
		}
	}

	if cfg["action_id"] == "" {
		return cfg, fmt.Errorf("未找到 action_id")
	}

	log.Info().Msg("config scan ok")
	return cfg, nil
}

// doScanViaCFBypass 通过 CF-Bypass Solver 过 Cloudflare 后获取页面 HTML，再提取配置
func (w *GrokWorker) doScanViaCFBypass(ctx context.Context, cfBypassURL string) (Config, error) {
	cfg := Config{
		"site_key":   "0x4AAAAAAAhr9JGVDZbrZOo0",
		"action_id":  "",
		"state_tree": "%5B%22%22%2C%7B%22children%22%3A%5B%22(app)%22%2C%7B%22children%22%3A%5B%22(auth)%22%2C%7B%22children%22%3A%5B%22sign-up%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2C%22%2Fsign-up%22%2C%22refresh%22%5D%7D%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D",
	}

	// 调用 cf-bypass-solver 的 /fetch 接口
	fetchURL := strings.TrimRight(cfBypassURL, "/") + "/fetch?url=" + url.QueryEscape("https://accounts.x.ai/sign-up")
	client := &http.Client{Timeout: 120 * time.Second} // 过盾需要等浏览器，给足时间
	req, err := http.NewRequestWithContext(ctx, "GET", fetchURL, nil)
	if err != nil {
		return cfg, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return cfg, fmt.Errorf("CF-Bypass fetch 请求失败: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK    bool   `json:"ok"`
		HTML  string `json:"html"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return cfg, fmt.Errorf("CF-Bypass 响应解析失败: %w", err)
	}
	if !result.OK || result.HTML == "" {
		return cfg, fmt.Errorf("CF-Bypass 获取页面失败: %s", result.Error)
	}

	html := result.HTML
	log.Info().Int("html_len", len(html)).Msg("bypass ok, extracting")

	// 提取 site_key
	if m := reSiteKey.FindStringSubmatch(html); len(m) > 1 {
		cfg["site_key"] = m[1]
	}

	// 提取 state_tree
	if m := reStateTree.FindStringSubmatch(html); len(m) > 1 {
		cfg["state_tree"] = m[1]
	}

	// 提取 JS bundle URL，搜索 action_id
	profile := browserProfiles[rand.Intn(len(browserProfiles))]
	scriptMatches := reScriptSrc.FindAllStringSubmatch(html, -1)
	for _, sm := range scriptMatches {
		if len(sm) < 2 {
			continue
		}
		jsURL := sm[1]
		if !strings.HasPrefix(jsURL, "http") {
			jsURL = "https://accounts.x.ai" + jsURL
		}

		jsReq, _ := http.NewRequestWithContext(ctx, "GET", jsURL, nil)
		jsReq.Header.Set("User-Agent", profile.UserAgent)
		jsResp, err := client.Do(jsReq)
		if err != nil {
			continue
		}
		jsBody, _ := io.ReadAll(jsResp.Body)
		jsResp.Body.Close()

		if m := reActionID.FindString(string(jsBody)); m != "" {
			cfg["action_id"] = m
			break
		}
	}

	if cfg["action_id"] == "" {
		return cfg, fmt.Errorf("CF-Bypass 过盾后仍未找到 action_id")
	}

	log.Info().Msg("bypass scan ok")
	return cfg, nil
}

// RegisterOne 执行一次完整的 Grok 注册流程
// 优先级: 协议注册（gRPC-web + Server Actions）→ Camoufox 浏览器降级（如已配置）
func (w *GrokWorker) RegisterOne(ctx context.Context, opts RegisterOpts) {
	// 随机延迟分散启动
	jitter := time.Duration(rand.Intn(3000)) * time.Millisecond
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	logf := func(format string, args ...interface{}) {
		select {
		case opts.LogCh <- fmt.Sprintf(format, args...):
		default:
		}
	}

	// 优先远程服务：配置了 grok_reg_url 就直接走远程，不跑本地协议
	if serviceURL := opts.Config["grok_reg_url"]; serviceURL != "" {
		logf("[*] 任务开始（远程服务模式）...")
		result, ok := w.registerViaGrokService(ctx, serviceURL, opts, logf)
		if ok {
			email, _ := result["email"].(string)
			opts.OnSuccess(email, result)
		} else if ctx.Err() != nil {
			return // context 取消（达标或用户停止），不计入失败
		} else {
			opts.OnFail()
		}
		return
	}

	// 先尝试协议注册（拦截失败回调，不立即上报，支持降级）
	succeeded := false
	protoOpts := opts
	protoOpts.OnSuccess = func(email string, data map[string]interface{}) {
		succeeded = true
		opts.OnSuccess(email, data)
	}
	protoOpts.OnFail = func() { /* 由外层决策是否降级 */ }

	w.registerViaProtocol(ctx, protoOpts, logf)

	if succeeded {
		return
	}

	// context 已取消（目标达成或用户停止），不再降级
	if ctx.Err() != nil {
		opts.OnFail()
		return
	}

	// 协议失败 → Camoufox 浏览器降级（如已配置）
	if browserRegURL := opts.Config["camoufox_reg_url"]; browserRegURL != "" {
		logf("[!] 协议注册失败，降级到浏览器模式...")
		w.registerViaBrowser(ctx, browserRegURL, opts)
		return
	}

	opts.OnFail()
}

// registerViaProtocol 协议注册主流程（gRPC-web + Server Actions）
func (w *GrokWorker) registerViaProtocol(ctx context.Context, opts RegisterOpts, logf func(string, ...interface{})) {
	// 选择浏览器指纹
	profile := browserProfiles[rand.Intn(len(browserProfiles))]

	logf("[*] 任务开始")

	// 获取配置（gRPC-web 模式不再需要 action_id / state_tree）
	siteKey := opts.Config["site_key"]

	// ─── 创建 HTTP 会话 ───
	client := w.makeHTTPClient(opts.Proxy, 30*time.Second)

	// 会话预热（获取 __cf_bm cookie）
	warmupReq, _ := http.NewRequestWithContext(ctx, "GET", "https://accounts.x.ai", nil)
	warmupReq.Header.Set("User-Agent", profile.UserAgent)
	if resp, err := client.Do(warmupReq); err == nil {
		resp.Body.Close()
	}

	// ─── Phase 1: 创建临时邮箱（多 provider 自动切换）───
	// 平台专属邮箱 provider 覆盖全局优先级
	if platformProviders := opts.Config["grok_email_providers"]; platformProviders != "" {
		opts.Config["email_provider_priority"] = platformProviders
	}
	mailProvider := tempmail.NewMultiProvider(opts.Config)
	email, mailMeta, err := mailProvider.GenerateEmail(ctx)
	if err != nil {
		logf("[-] 创建邮箱失败: %s", err)
		opts.OnFail()
		return
	}
	password := randomString(15, "abcdefghijklmnopqrstuvwxyz0123456789")
	logf("[*] 邮箱: %s (via %s)", email, mailMeta["provider"])

	// 注册失败时清理邮箱
	defer func() {
		go mailProvider.DeleteEmail(context.Background(), email, mailMeta)
	}()

	// ─── Phase 2: 发送验证码 (gRPC-web) ───
	logf("[*] 发送验证码...")
	grpcBody := grpcweb.EncodeEmailCode(email)
	sendResp, err := w.doGRPCWeb(ctx, client, profile.UserAgent,
		"https://accounts.x.ai/auth_mgmt.AuthManagement/CreateEmailValidationCode",
		"https://accounts.x.ai", "https://accounts.x.ai/sign-up?redirect=grok-com",
		grpcBody, nil)
	if err != nil || sendResp.StatusCode != 200 {
		status := 0
		if sendResp != nil {
			status = sendResp.StatusCode
			sendResp.Body.Close()
		}
		logf("[-] 发送验证码失败: HTTP %d, %v", status, err)
		opts.OnFail()
		return
	}
	sendResp.Body.Close()
	logf("[+] 验证码已发送")

	// ─── Phase 3: 获取验证码 ───
	logf("[*] 等待验证码（最长 30s）...")
	var code string
	for attempt := 1; attempt <= 30; attempt++ {
		select {
		case <-ctx.Done():
			logf("[-] 等待验证码时任务被取消")
			opts.OnFail()
			return
		case <-time.After(1 * time.Second):
		}
		c, err := mailProvider.FetchVerificationCode(ctx, email, mailMeta, 1, 0)
		if err == nil && c != "" {
			code = c
			break
		}
		if attempt%5 == 0 {
			logf("[*] 等待验证码中... 已等待 %ds", attempt)
		}
	}
	if code == "" {
		logf("[-] 获取验证码超时")
		tempmail.RecordFailure(mailMeta["provider"])
		opts.OnFail()
		return
	}
	logf("[+] 验证码: %s", code)
	tempmail.RecordSuccess(mailMeta["provider"])

	// ─── Phase 4: 验证邮箱 (gRPC-web) ───
	verifyBody := grpcweb.EncodeVerifyCode(email, code)
	verifyResp, err := w.doGRPCWeb(ctx, client, profile.UserAgent,
		"https://accounts.x.ai/auth_mgmt.AuthManagement/VerifyEmailValidationCode",
		"https://accounts.x.ai", "https://accounts.x.ai/sign-up?redirect=grok-com",
		verifyBody, nil)
	if err != nil || verifyResp.StatusCode != 200 {
		if verifyResp != nil {
			verifyResp.Body.Close()
		}
		logf("[-] 验证邮箱失败")
		opts.OnFail()
		return
	}
	verifyResp.Body.Close()
	logf("[+] 邮箱验证成功")

	// ─── Phase 5+6: Server Actions 注册 ───
	firstName := randomName(4, 6)
	lastName := randomName(4, 6)

	var solverURLs []string
	if u := opts.Config["turnstile_solver_url"]; u != "" {
		solverURLs = append(solverURLs, u)
	}
	if u := opts.Config["cf_bypass_solver_url"]; u != "" {
		solverURLs = append(solverURLs, u)
	}
	// 从代理池提取代理地址，传给 Turnstile solver 浏览器使用
	var solverProxyURL string
	if opts.Proxy != nil {
		if opts.Proxy.HTTPS != "" {
			solverProxyURL = opts.Proxy.HTTPS
		} else if opts.Proxy.HTTP != "" {
			solverProxyURL = opts.Proxy.HTTP
		}
	}
	solver := turnstile.NewSolver(solverURLs, opts.Config["capsolver_key"], opts.Config["yescaptcha_key"], solverProxyURL)

	actionID := opts.Config["action_id"]
	stateTree := settingOrDefault(opts.Config, "state_tree", defaultStateTree)

	if actionID == "" {
		logf("[-] 缺少注册配置，无法注册")
		opts.OnFail()
		return
	}

	var ssoToken string
	registered := false

	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-ctx.Done():
			opts.OnFail()
			return
		default:
		}

		logf("[*] 注册尝试 %d/3...", attempt+1)

		logf("[*] 验证远程服务接口...")
		turnstileToken, err := solver.Solve(ctx, "https://accounts.x.ai/sign-up", siteKey, 3, logf)
		if err != nil {
			logf("[-] 验证失败: %s", err)
			ctxSleep(ctx, 3*time.Second)
			continue
		}
		logf("[+] 卧槽Σ(°ロ°)验证通过 (%d chars)", len(turnstileToken))

		regPayload := []map[string]interface{}{{
			"emailValidationCode": code,
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
			ctxSleep(ctx, 3*time.Second)
			continue
		}
		respBody, _ := io.ReadAll(regResp.Body)
		regResp.Body.Close()
		respText := string(respBody)

		if regResp.StatusCode != 200 {
			logf("[-] 注册响应: HTTP %d, body=%s", regResp.StatusCode, truncStr(respText, 200))
			ctxSleep(ctx, 3*time.Second)
			continue
		}

		var cookieURLMatch string
		if sm := reCookieURLAnchored.FindStringSubmatch(respText); len(sm) > 1 {
			cookieURLMatch = sm[1]
			logf("[*] 会话处理中...")
		} else if sm := reCookieURLQuoted.FindStringSubmatch(respText); len(sm) > 1 {
			cookieURLMatch = sm[1]
			logf("[*] 会话处理中（备用）...")
		}
		if cookieURLMatch == "" {
			logf("[-] 会话处理失败，响应异常")
			ctxSleep(ctx, 3*time.Second)
			continue
		}

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
		logf("[*] 会话回调完成: HTTP %d", cookieResp.StatusCode)

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
				for _, c := range client.Jar.Cookies(domainURL) {
					if c.Name == "sso" && ssoToken == "" {
						ssoToken = c.Value
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
		opts.OnFail()
		return
	}

	// ─── Phase 7+8: TOS + 生日 + NSFW + Unhinged + PayUrl ───
	logf("[*] 马上处理..设置中...")
	nsfwEnabled, checkoutURL := w.enableNSFW(ctx, ssoToken, email, opts.Proxy, logf)
	if nsfwEnabled {
		logf("[+] 马上处理..成")
	} else {
		logf("[!] 马上处理..失败（账号已创建，继续保存）")
	}

	result := map[string]interface{}{
		"auth_token":      ssoToken,
		"feature_enabled": nsfwEnabled,
	}
	if checkoutURL != "" {
		result["redirect_url"] = checkoutURL
	}
	// 保存邮箱 provider 信息，供后续 FetchOTP 使用
	result["provider"] = mailMeta["provider"]
	emailMetaCopy := make(map[string]interface{}, len(mailMeta))
	for k, v := range mailMeta {
		emailMetaCopy[k] = v
	}
	result["email_meta"] = emailMetaCopy
	logf("[OK] 任务完成: %s", email)
	opts.OnSuccess(email, result)
}

// ─── 内部方法 ───

// ─── NSFW 配置 ───

// enableNSFW 设置 TOS + 生日 + NSFW + Unhinged + PayUrl
// 优先 Go 原生（标准 HTTP），失败时自动降级到 Python curl_cffi
func (w *GrokWorker) enableNSFW(ctx context.Context, ssoToken, email string, proxy *ProxyEntry, logf func(string, ...interface{})) (bool, string) {
	ok, checkoutURL := w.enableNSFWGo(ctx, ssoToken, email, proxy, logf)
	if ok {
		return true, checkoutURL
	}
	logf("[!] Go 原生配置失败，降级到备用方案...")
	return w.enableNSFWViaPython(ctx, ssoToken, email, proxy, logf)
}

// enableNSFWGo 纯 Go 实现 NSFW 配置（不依赖 Python / curl_cffi）
// 流程: 预热 grok.com → TOS(accounts.x.ai) → 生日 → NSFW → Unhinged → PayUrl
// HF Space 出口 IP 干净，Go 标准 net/http 通常可以直通 Cloudflare
func (w *GrokWorker) enableNSFWGo(ctx context.Context, ssoToken, email string, proxy *ProxyEntry, logf func(string, ...interface{})) (bool, string) {
	client := w.makeHTTPClient(proxy, 20*time.Second)
	profile := browserProfiles[rand.Intn(len(browserProfiles))]

	// 设置 SSO cookie 到 cookie jar
	for _, domain := range []string{"https://grok.com", "https://accounts.x.ai"} {
		domainURL, _ := url.Parse(domain)
		client.Jar.SetCookies(domainURL, []*http.Cookie{
			{Name: "sso", Value: ssoToken},
			{Name: "sso-rw", Value: ssoToken},
		})
	}

	// 预热 grok.com（获取 __cf_bm cookie）
	warmupReq, _ := http.NewRequestWithContext(ctx, "GET", "https://grok.com", nil)
	warmupReq.Header.Set("User-Agent", profile.UserAgent)
	if resp, err := client.Do(warmupReq); err == nil {
		resp.Body.Close()
		logf("[*] 预热: HTTP %d", resp.StatusCode)
		if resp.StatusCode == 403 {
			logf("[-] 预热被拦截，放弃")
			return false, ""
		}
	} else {
		logf("[-] 预热失败: %s", err)
		return false, ""
	}

	// Step 1: TOS (accounts.x.ai gRPC-web)
	tosPayload := grpcWebWrap([]byte{0x10, 0x01}) // field 2 (tos_version) = 1
	tosResp, err := w.doGRPCWeb(ctx, client, profile.UserAgent,
		"https://accounts.x.ai/auth_mgmt.AuthManagement/SetTosAcceptedVersion",
		"https://accounts.x.ai", "https://accounts.x.ai/accept-tos",
		tosPayload, nil)
	if err != nil {
		logf("[-] 协议请求失败: %s", err)
		return false, ""
	}
	tosResp.Body.Close()
	if tosResp.StatusCode != 200 {
		logf("[-] 协议响应异常: %d", tosResp.StatusCode)
		return false, ""
	}
	logf("[+] 协议已接受")

	// Step 2: 生日 (grok.com REST)
	bdPayload, _ := json.Marshal(map[string]string{"birthDate": randomBirthDate()})
	bdReq, _ := http.NewRequestWithContext(ctx, "POST", "https://grok.com/rest/auth/set-birth-date", bytes.NewReader(bdPayload))
	bdReq.Header.Set("Content-Type", "application/json")
	bdReq.Header.Set("Origin", "https://grok.com")
	bdReq.Header.Set("Referer", "https://grok.com/")
	bdReq.Header.Set("User-Agent", profile.UserAgent)
	bdResp, err := client.Do(bdReq)
	if err != nil {
		logf("[-] 信息设置失败: %s", err)
		return false, ""
	}
	bdResp.Body.Close()
	if bdResp.StatusCode != 200 {
		logf("[-] 信息设置异常: %d", bdResp.StatusCode)
		return false, ""
	}
	logf("[+] 信息设置成功")

	// Step 3: NSFW (grok.com gRPC-web)
	// protobuf: field 1 = {field 2 = 1} + field 2 = "always_show_nsfw_content"
	nsfwStr := []byte("always_show_nsfw_content")
	field1 := []byte{0x0a, 0x02, 0x10, 0x01}
	field2Inner := append([]byte{0x0a, byte(len(nsfwStr))}, nsfwStr...)
	field2 := append([]byte{0x12, byte(len(field2Inner))}, field2Inner...)
	nsfwPayload := grpcWebWrap(append(field1, field2...))
	nsfwResp, err := w.doGRPCWeb(ctx, client, profile.UserAgent,
		"https://grok.com/auth_mgmt.AuthManagement/UpdateUserFeatureControls",
		"https://grok.com", "https://grok.com/",
		nsfwPayload, nil)
	if err != nil {
		logf("[-] 偏好设置请求失败: %s", err)
		return false, ""
	}
	nsfwResp.Body.Close()
	if nsfwResp.StatusCode != 200 {
		logf("[-] 偏好设置异常: %d", nsfwResp.StatusCode)
		return false, ""
	}
	logf("[+] 偏好设置成功")

	// Step 4: Unhinged (grok.com gRPC-web, 不阻断主流程)
	// protobuf: field 1 = 1, field 2 = 1
	unhingedPayload := grpcWebWrap([]byte{0x08, 0x01, 0x10, 0x01})
	unhingedResp, err := w.doGRPCWeb(ctx, client, profile.UserAgent,
		"https://grok.com/auth_mgmt.AuthManagement/UpdateUserFeatureControls",
		"https://grok.com", "https://grok.com/",
		unhingedPayload, nil)
	if err == nil && unhingedResp != nil {
		unhingedResp.Body.Close()
		if unhingedResp.StatusCode == 200 {
			logf("[+] 高级模式已开启")
		} else {
			logf("[-] 高级模式设置异常: %d（不阻断）", unhingedResp.StatusCode)
		}
	} else {
		logf("[-] 高级模式请求失败（不阻断）: %v", err)
	}

	// Step 5: PayUrl 支付链接（不阻断主流程）
	checkoutURL := ""
	if email != "" {
		checkoutURL = w.createCheckoutURLGo(ctx, client, profile.UserAgent, email, logf)
	}

	return true, checkoutURL
}

// createCheckoutURLGo 纯 Go 生成 Grok Pro Stripe 订阅支付链接
func (w *GrokWorker) createCheckoutURLGo(ctx context.Context, client *http.Client, ua, email string, logf func(string, ...interface{})) string {
	// Step 1: 创建 Stripe 客户
	custPayload, _ := json.Marshal(map[string]interface{}{
		"billingInfo": map[string]string{
			"name":  randomName(5, 8) + " " + randomName(5, 8),
			"email": email,
		},
	})
	custReq, _ := http.NewRequestWithContext(ctx, "POST",
		"https://grok.com/rest/subscriptions/customer/new", bytes.NewReader(custPayload))
	custReq.Header.Set("Content-Type", "application/json")
	custReq.Header.Set("Origin", "https://grok.com")
	custReq.Header.Set("Referer", "https://grok.com/")
	custReq.Header.Set("User-Agent", ua)
	custReq.Header.Set("X-Xai-Request-Id", randomUUID())
	custResp, err := client.Do(custReq)
	if err != nil {
		logf("[-] 支付初始化失败: %s", err)
		return ""
	}
	custResp.Body.Close()
	if custResp.StatusCode != 200 && custResp.StatusCode != 201 && custResp.StatusCode != 204 {
		logf("[-] 支付初始化异常: %d", custResp.StatusCode)
		return ""
	}

	// Step 2: 创建订阅获取 checkout URL
	subPayload, _ := json.Marshal(map[string]interface{}{
		"stripeHosted": map[string]string{
			"successUrl": "https://grok.com/?checkout=success&tier=SUBSCRIPTION_TIER_GROK_PRO&interval=monthly#subscribe",
		},
		"priceId":                          "price_1R6nQ9HJohyvID2ck7FNrVdw",
		"campaignId":                       "subcamp_HeAxW",
		"ignoreExistingActiveSubscriptions": false,
		"subscriptionType":                 "MONTHLY",
		"requestedTier":                    "REQUESTED_TIER_GROK_PRO",
	})
	subReq, _ := http.NewRequestWithContext(ctx, "POST",
		"https://grok.com/rest/subscriptions/subscribe/new", bytes.NewReader(subPayload))
	subReq.Header.Set("Content-Type", "application/json")
	subReq.Header.Set("Origin", "https://grok.com")
	subReq.Header.Set("Referer", "https://grok.com/")
	subReq.Header.Set("User-Agent", ua)
	subReq.Header.Set("X-Xai-Request-Id", randomUUID())
	subResp, err := client.Do(subReq)
	if err != nil {
		logf("[-] 订阅创建失败: %s", err)
		return ""
	}
	defer subResp.Body.Close()
	if subResp.StatusCode != 200 {
		logf("[-] 订阅创建异常: %d", subResp.StatusCode)
		return ""
	}

	var subResult map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(subResp.Body, 64*1024)).Decode(&subResult); err != nil {
		logf("[-] 订阅响应解析失败: %s", err)
		return ""
	}
	if u, ok := subResult["url"].(string); ok && u != "" {
		logf("[+] 支付链接获取成功")
		return u
	}
	if u, ok := subResult["checkoutUrl"].(string); ok && u != "" {
		logf("[+] 支付链接获取成功")
		return u
	}
	logf("[-] 支付链接未找到")
	return ""
}

// enableNSFWViaPython 通过 Python curl_cffi 子进程设置 TOS + 生日 + NSFW + Unhinged + PayUrl
// Go utls 无法通过 grok.com 的 Cloudflare（HTTP/2 不支持导致 connection reset）
// Python curl_cffi impersonate="chrome120" 完整支持 TLS + HTTP/2
// 返回 (nsfw成功, checkout_url)
func (w *GrokWorker) enableNSFWViaPython(ctx context.Context, ssoToken, email string, proxy *ProxyEntry, logf func(string, ...interface{})) (bool, string) {
	// 查找 enable_nsfw.py 脚本
	scriptPath := ""
	candidates := []string{
		"scripts/enable_nsfw.py",
	}
	// 也检查可执行文件相对路径
	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		candidates = append(candidates,
			filepath.Join(execDir, "scripts", "enable_nsfw.py"),
			filepath.Join(execDir, "..", "scripts", "enable_nsfw.py"),
		)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			scriptPath = p
			break
		}
	}
	if scriptPath == "" {
		logf("[-] 马上处理..组件未找到")
		return false, ""
	}

	// 调用: python3 enable_nsfw.py --sso <TOKEN> --email <EMAIL>
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "python3", scriptPath, "--sso", ssoToken)

	// 传递代理配置给 Python 脚本
	if proxy != nil {
		proxyStr := proxy.HTTPS
		if proxyStr == "" {
			proxyStr = proxy.HTTP
		}
		if proxyStr != "" {
			cmd.Args = append(cmd.Args, "--proxy", proxyStr)
		}
	}

	// 传递 email 以生成 PayUrl checkout 链接
	if email != "" {
		cmd.Args = append(cmd.Args, "--email", email)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	outStr := strings.TrimSpace(stdout.String())
	errStr := strings.TrimSpace(stderr.String())

	// 打印 Python 脚本调试日志（stderr 里有各步骤的 HTTP 状态码）
	if errStr != "" {
		for _, line := range strings.Split(errStr, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				logf("[*] %s", line)
			}
		}
	}

	if err != nil {
		logf("[!] 马上处理..失败: %s", truncStr(outStr, 200))
		return false, ""
	}

	// 解析输出：NSFW_OK:<msg>|<checkout_url> 或 NSFW_FAIL:<reason>
	checkoutURL := ""
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "NSFW_OK:") {
			payload := strings.TrimPrefix(line, "NSFW_OK:")
			// 格式: <msg>|<checkout_url>（checkout_url 可为空）
			parts := strings.SplitN(payload, "|", 2)
			logf("[+] %s", parts[0])
			if len(parts) == 2 && parts[1] != "" {
				checkoutURL = parts[1]
				logf("[+] PayUrl: %s", truncStr(checkoutURL, 80))
			}
			return true, checkoutURL
		}
		if strings.HasPrefix(line, "NSFW_FAIL:") {
			logf("[-] 马上处理..失败: %s", strings.TrimPrefix(line, "NSFW_FAIL:"))
			return false, ""
		}
	}

	logf("[!] 马上处理..异常: %s", truncStr(outStr, 200))
	return false, ""
}

// doGRPCWeb 发送 gRPC-web 请求
func (w *GrokWorker) doGRPCWeb(ctx context.Context, client *http.Client, ua, endpoint, origin, referer string, body []byte, extraCookies map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("X-Grpc-Web", "1")
	req.Header.Set("X-User-Agent", "connect-es/2.1.1")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", ua)

	// 将额外 cookie 添加到 jar（保留 jar 已有的 __cf_bm 等 Cloudflare cookie）
	if len(extraCookies) > 0 {
		var cookies []*http.Cookie
		for k, v := range extraCookies {
			cookies = append(cookies, &http.Cookie{Name: k, Value: v})
		}
		client.Jar.SetCookies(req.URL, cookies)
	}

	return client.Do(req)
}

// makeHTTPClient 创建带代理和 cookie jar 的 HTTP 客户端
func (w *GrokWorker) makeHTTPClient(proxy *ProxyEntry, timeout time.Duration) *http.Client {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  false,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	applyProxy(transport, proxy)

	return &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   timeout,
	}
}

// ─── 工具函数 ───

// grpcWebWrap 将 protobuf payload 包装为 gRPC-web 帧（0x00 + 4字节大端长度 + payload）
func grpcWebWrap(payload []byte) []byte {
	buf := make([]byte, 5+len(payload))
	buf[0] = 0x00
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

// randomBirthDate 生成随机出生日期（20-40 岁之间）
func randomBirthDate() string {
	now := time.Now()
	age := 20 + rand.Intn(21)
	return fmt.Sprintf("%d-%02d-%02dT16:00:00.000Z", now.Year()-age, 1+rand.Intn(12), 1+rand.Intn(28))
}

// randomUUID 生成随机 UUID v4（用于 X-Xai-Request-Id 请求头）
func randomUUID() string {
	var buf [16]byte
	cryptorand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 2
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func randomString(length int, charset string) string {
	b := make([]byte, length)
	for i := range b {
		n, _ := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(len(charset))))
		b[i] = charset[n.Int64()]
	}
	return string(b)
}

func randomName(minLen, maxLen int) string {
	n, _ := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maxLen-minLen+1)))
	length := minLen + int(n.Int64())
	name := randomString(length, "abcdefghijklmnopqrstuvwxyz")
	return strings.ToUpper(name[:1]) + name[1:]
}

func settingOrDefault(cfg Config, key, fallback string) string {
	if v, ok := cfg[key]; ok && v != "" {
		return v
	}
	return fallback
}

// truncStr 截断字符串用于日志输出
func truncStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// filterLocalURLs 从逗号分隔的 URL 列表中过滤掉本地地址（127.0.0.1/localhost/host.docker.internal）
// HF Space 无法访问 VPS 本地服务，只保留外部可达的 URL
func filterLocalURLs(urls string) string {
	if urls == "" {
		return ""
	}
	parts := strings.Split(urls, ",")
	remote := make([]string, 0, len(parts))
	for _, u := range parts {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if strings.Contains(u, "127.0.0.1") || strings.Contains(u, "localhost") || strings.Contains(u, "host.docker.internal") {
			continue
		}
		remote = append(remote, u)
	}
	return strings.Join(remote, ",")
}

// ─── 远程服务模式 ───

// grokServiceRequest VPS → HF 远程注册请求
type grokServiceRequest struct {
	Proxy                string `json:"proxy,omitempty"`
	YYDSMailURL          string `json:"yydsmail_url,omitempty"`
	YYDSMailKey          string `json:"yydsmail_key,omitempty"`
	EmailPriority        string `json:"email_priority,omitempty"`
	TurnstileSolverURL   string `json:"turnstile_solver_url,omitempty"`
	TurnstileSolverProxy string `json:"turnstile_solver_proxy,omitempty"`
	CFBypassSolverURL    string `json:"cf_bypass_solver_url,omitempty"`
	CapSolverKey         string `json:"capsolver_key,omitempty"`
	YesCaptchaKey        string `json:"yescaptcha_key,omitempty"`
	SiteKey              string `json:"site_key,omitempty"`
	ActionID             string `json:"action_id,omitempty"`
	StateTree            string `json:"state_tree,omitempty"`
}

// grokServiceResponse HF → VPS 远程注册响应
type grokServiceResponse struct {
	OK             bool     `json:"ok"`
	Email          string   `json:"email"`
	SSOToken       string   `json:"auth_token"`
	NSFWEnabled    bool     `json:"feature_enabled"`
	CheckoutURL    string   `json:"redirect_url"`
	EmailProvider  string   `json:"provider"`
	Error          string   `json:"error"`
	Logs           []string `json:"logs"`
}

// registerViaGrokService 通过远程 HF 服务完成完整 Grok 注册
// 请求链: VPS → CF Worker → HFGS (grok-worker 二进制)
func (w *GrokWorker) registerViaGrokService(ctx context.Context, serviceURL string, opts RegisterOpts, logf func(string, ...interface{})) (map[string]interface{}, bool) {
	logf("[*] 远程服务连接中...")

	proxyStr := ""
	if userProxy := opts.Config["user_proxy"]; userProxy != "" {
		proxyStr = userProxy
		logf("[*] 使用用户指定代理")
	} else if opts.Proxy != nil {
		if opts.Proxy.HTTPS != "" {
			proxyStr = opts.Proxy.HTTPS
		} else if opts.Proxy.HTTP != "" {
			proxyStr = opts.Proxy.HTTP
		}
	}
	// HF Space 无法访问 VPS 本地代理，本地地址直接丢弃
	// HF 出口 IP 干净，不带代理通常可以直通 Cloudflare
	if strings.Contains(proxyStr, "127.0.0.1") || strings.Contains(proxyStr, "localhost") || strings.Contains(proxyStr, "host.docker.internal") {
		proxyStr = ""
	}

	// 优先使用平台级邮箱选择，为空时回退到全局优先级
	emailPriority := opts.Config["grok_email_providers"]
	if emailPriority == "" {
		emailPriority = settingOrDefault(opts.Config, "email_provider_priority", "yydsmail")
	}
	epParts := strings.Split(emailPriority, ",")
	for i := range epParts {
		epParts[i] = strings.TrimSpace(epParts[i])
	}
	tempmail.WeightedShuffleNames(epParts)

	// 诊断日志：发送到远程的配置
	logf("[*] 邮箱优先级: %s", strings.Join(epParts, ","))
	if opts.Config["yydsmail_api_key"] != "" {
		logf("[*] yydsmail 凭证: 已配置")
	}

	reqBody := grokServiceRequest{
		Proxy:                proxyStr,
		YYDSMailURL:          settingOrDefault(opts.Config, "yydsmail_base_url", ""),
		YYDSMailKey:          opts.Config["yydsmail_api_key"],
		EmailPriority:        strings.Join(epParts, ","),
		TurnstileSolverURL:   filterLocalURLs(opts.Config["turnstile_solver_url"]),
		TurnstileSolverProxy: opts.Config["turnstile_solver_proxy"],
		CFBypassSolverURL:    filterLocalURLs(opts.Config["cf_bypass_solver_url"]),
		CapSolverKey:         opts.Config["capsolver_key"],
		YesCaptchaKey:        opts.Config["yescaptcha_key"],
		SiteKey:              opts.Config["site_key"],
		ActionID:             opts.Config["action_id"],
		StateTree:            settingOrDefault(opts.Config, "state_tree", defaultStateTree),
	}
	bodyJSON, _ := json.Marshal(reqBody)

	// 600s 超时：排队等待（最多 ~300s）+ Turnstile + 注册 + 验证码（120-180s）+ 余量
	reqCtx, reqCancel := context.WithTimeout(ctx, 600*time.Second)
	defer reqCancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, "POST",
		strings.TrimRight(serviceURL, "/")+"/grok/process",
		bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, false
	}
	httpReq.Header.Set("Content-Type", "application/json")

	logf("[*] 请求已发送，等待远程注册...")

	// 心跳：每 10 秒轮询排队状态，向用户推送排队位置/ETA
	heartbeatDone := make(chan struct{})
	queueStatusURL := BuildQueueStatusURL(serviceURL, "grok")
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		elapsed := 0
		for {
			select {
			case <-ticker.C:
				elapsed += 10
				qs, err := PollQueueStatus(reqCtx, queueStatusURL)
				if err == nil && qs != nil {
					logf("%s", FormatQueueLog(qs, elapsed))
				} else {
					logf("[…] 远程注册进行中 (%ds)...", elapsed)
				}
			case <-heartbeatDone:
				return
			case <-reqCtx.Done():
				return
			}
		}
	}()

	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := client.Do(httpReq)
	close(heartbeatDone)

	if err != nil {
		if ctx.Err() != nil {
			return nil, false // context 取消（达标或用户停止），静默退出
		}
		logf("[-] 远程服务请求失败: %s", sanitizeHTTPError(err))
		return nil, false
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024)) // 256KB 上限

	// HTTP 状态码检查：非 2xx 直接报错，不尝试 JSON 解析
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		logf("[-] 远程服务返回 HTTP %d: %s", resp.StatusCode, snippet)
		return nil, false
	}

	var svcResp grokServiceResponse
	if err := json.Unmarshal(body, &svcResp); err != nil {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		logf("[-] 响应解析失败 (HTTP %d): %s | body: %s", resp.StatusCode, err, snippet)
		return nil, false
	}

	// 转发远程服务的详细日志
	for _, remoteLog := range svcResp.Logs {
		logf("  %s", remoteLog)
	}

	// 从远程日志中提取邮箱 provider 名，回写亲和度统计
	if provider := extractProviderFromRemoteLogs(svcResp.Logs); provider != "" {
		if svcResp.OK {
			tempmail.RecordSuccess(provider)
			logf("[*] 亲和度: %s +success", provider)
		} else if isEmailRelatedFailure(svcResp.Error) {
			tempmail.RecordFailure(provider)
			logf("[*] 亲和度: %s +fail (邮箱相关)", provider)
		}
	}

	if !svcResp.OK {
		logf("[-] 远程注册失败: %s", svcResp.Error)
		return nil, false
	}

	logf("[+] 远程 Grok 注册成功: %s", svcResp.Email)

	// 解析完整 JSON 到 map，保留远程服务返回的所有字段
	result := map[string]interface{}{}
	json.Unmarshal(body, &result) //nolint:errcheck
	delete(result, "ok")
	delete(result, "error")
	delete(result, "logs")

	return result, true
}

// ─── 浏览器模式降级 ───

// browserRegRequest Camoufox 注册服务请求体
type browserRegRequest struct {
	Email   string `json:"email"`
	Proxy   string `json:"proxy,omitempty"`
	SiteKey string `json:"site_key,omitempty"`
}

// browserRegResponse Camoufox 注册服务响应体
type browserRegResponse struct {
	OK         bool   `json:"ok"`
	SSOToken   string `json:"auth_token"`
	NSFWEnable bool   `json:"feature_enabled"`
	Error      string `json:"error,omitempty"`
}

// registerViaBrowser 通过 Camoufox 浏览器注册服务完成注册
func (w *GrokWorker) registerViaBrowser(ctx context.Context, browserRegURL string, opts RegisterOpts) {
	logf := func(format string, args ...interface{}) {
		select {
		case opts.LogCh <- fmt.Sprintf(format, args...):
		default:
		}
	}

	logf("[*] 浏览器模式注册开始")

	// 创建临时邮箱（多 provider 自动切换）
	// 平台专属邮箱 provider 覆盖全局优先级（Camoufox 模式复用同一逻辑）
	if platformProviders := opts.Config["grok_email_providers"]; platformProviders != "" {
		opts.Config["email_provider_priority"] = platformProviders
	}
	mailProvider := tempmail.NewMultiProvider(opts.Config)
	email, mailMeta, err := mailProvider.GenerateEmail(ctx)
	if err != nil {
		logf("[-] 创建邮箱失败: %s", err)
		opts.OnFail()
		return
	}
	logf("[*] 邮箱: %s (via %s)", email, mailMeta["provider"])
	defer func() { go mailProvider.DeleteEmail(context.Background(), email, mailMeta) }()

	// 构建代理字符串
	proxyStr := ""
	if userProxy := opts.Config["user_proxy"]; userProxy != "" {
		proxyStr = userProxy
		logf("[*] 使用用户指定代理")
	} else if opts.Proxy != nil {
		if opts.Proxy.HTTPS != "" {
			proxyStr = opts.Proxy.HTTPS
		} else if opts.Proxy.HTTP != "" {
			proxyStr = opts.Proxy.HTTP
		}
	}

	// 调用 Camoufox 注册服务
	reqBody := browserRegRequest{
		Email:   email,
		Proxy:   proxyStr,
		SiteKey: opts.Config["site_key"],
	}
	bodyJSON, _ := json.Marshal(reqBody)

	// 浏览器注册耗时较长，但必须关联任务 ctx，停止任务时立即取消
	reqCtx, reqCancel := context.WithTimeout(ctx, 210*time.Second)
	defer reqCancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST",
		strings.TrimRight(browserRegURL, "/")+"/grok/process",
		bytes.NewReader(bodyJSON))
	if err != nil {
		logf("[-] 构建请求失败: %s", err)
		opts.OnFail()
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// 浏览器注册可能耗时较长，使用较长超时
	client := &http.Client{Timeout: 210 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		logf("[-] 浏览器注册服务请求失败: %s", err)
		opts.OnFail()
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024)) // 256KB 上限
	var result browserRegResponse
	if err := json.Unmarshal(body, &result); err != nil {
		logf("[-] 浏览器注册服务响应解析失败 (HTTP %d): %s",
			resp.StatusCode, err)
		opts.OnFail()
		return
	}

	if !result.OK || result.SSOToken == "" {
		logf("[-] 浏览器注册失败: %s", result.Error)
		opts.OnFail()
		return
	}

	logf("[OK] 浏览器注册完成: %s", email)

	if result.NSFWEnable {
		// 解析完整 JSON，保留远程服务返回的所有字段
		fullResult := map[string]interface{}{}
		json.Unmarshal(body, &fullResult) //nolint:errcheck
		delete(fullResult, "ok")
		delete(fullResult, "error")
		// 保存邮箱 provider 信息，供后续 FetchOTP 使用
		fullResult["provider"] = mailMeta["provider"]
		tempmail.RecordSuccess(mailMeta["provider"])
		emailMetaCopy := make(map[string]interface{}, len(mailMeta))
		for k, v := range mailMeta {
			emailMetaCopy[k] = v
		}
		fullResult["email_meta"] = emailMetaCopy
		opts.OnSuccess(email, fullResult)
	} else {
		logf("[!] 马上处理..未完成")
		opts.OnFail()
	}
}
