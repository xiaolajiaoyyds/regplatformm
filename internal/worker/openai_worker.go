package worker

import (
	"bufio"
	"bytes"
	"context"
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
	"strings"
	"time"

	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"math/big"

	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/sentinel"
	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/tempmail"
	"golang.org/x/net/publicsuffix"
)

// OpenAIWorker OpenAI 平台注册器（纯 HTTP 协议注册）
type OpenAIWorker struct{}

func init() {
	Register(&OpenAIWorker{})
}

func (w *OpenAIWorker) PlatformName() string { return "openai" }

func (w *OpenAIWorker) ScanConfig(ctx context.Context, proxy *ProxyEntry, cfg Config) (Config, error) {
	if cfg["yydsmail_api_key"] == "" {
		return Config{}, fmt.Errorf("缺少 yydsmail_api_key 配置")
	}
	return Config{}, nil
}

func (w *OpenAIWorker) RegisterOne(ctx context.Context, opts RegisterOpts) {
	// 随机延迟分散启动
	jitter := time.Duration(rand.Intn(2000)) * time.Millisecond
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

	// 优先远程服务：配置了 openai_reg_url 就直接走远程，不跑本地协议
	if serviceURL := opts.Config["openai_reg_url"]; serviceURL != "" {
		fmt.Println("[DEBUG] OpenAI 注册开始（远程服务模式）")
		result, ok := w.registerViaService(ctx, serviceURL, opts, logf)
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

	// 未配置远程服务，走本地协议注册
	fmt.Println("[DEBUG] OpenAI 注册开始（本地协议模式）")

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

	if ctx.Err() != nil {
		opts.OnFail()
		return
	}

	opts.OnFail()
}

// ─── 纯 HTTP 协议注册 ───

const (
	openaiAuthBase   = "https://auth.openai.com"
	oauthClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthRedirectURI = "http://localhost:1455/auth/callback"
	openaiConsentURL = openaiAuthBase + "/sign-in-with-chatgpt/codex/consent"
)

// chromeProfile 完整的浏览器指纹配置（与 codex.py _CHROME_PROFILES 对齐）
type chromeProfile struct {
	ua             string // navigator.userAgent
	secChUA        string // Sec-Ch-Ua 短版本
	secChUAFull    string // Sec-Ch-Ua-Full-Version-List
	secChUAPlatVer string // Sec-Ch-Ua-Platform-Version（Windows 版本号）
}

// chromeProfiles 4 个真实 Chrome 版本指纹（131/133/136/142）
// sec-ch-ua brand string 格式各不相同，与 codex.py 保持一致
var chromeProfiles = []chromeProfile{
	{
		ua:             "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		secChUA:        `"Not A;Brand";v="99", "Chromium";v="131", "Google Chrome";v="131"`,
		secChUAFull:    `"Not A;Brand";v="99.0.0.0", "Chromium";v="131.0.6778.264", "Google Chrome";v="131.0.6778.264"`,
		secChUAPlatVer: "15.0.0",
	},
	{
		ua:             "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
		secChUA:        `"Not(A:Brand";v="99", "Chromium";v="133", "Google Chrome";v="133"`,
		secChUAFull:    `"Not(A:Brand";v="99.0.0.0", "Chromium";v="133.0.6943.98", "Google Chrome";v="133.0.6943.98"`,
		secChUAPlatVer: "15.0.0",
	},
	{
		ua:             "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
		secChUA:        `"Not.A/Brand";v="8", "Chromium";v="136", "Google Chrome";v="136"`,
		secChUAFull:    `"Not.A/Brand";v="8.0.0.0", "Chromium";v="136.0.7103.93", "Google Chrome";v="136.0.7103.93"`,
		secChUAPlatVer: "15.0.0",
	},
	{
		ua:             "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36",
		secChUA:        `"Not_A Brand";v="99", "Chromium";v="142", "Google Chrome";v="142"`,
		secChUAFull:    `"Not_A Brand";v="99.0.0.0", "Chromium";v="142.0.7348.0", "Google Chrome";v="142.0.7348.0"`,
		secChUAPlatVer: "15.0.0",
	},
}

// acceptLanguages 常见英语语言标头池（随机选择增加多样性）
var acceptLanguages = []string{
	"en-US,en;q=0.9",
	"en-US,en;q=0.9,zh-CN;q=0.8",
	"en,en-US;q=0.9",
	"en-US,en;q=0.8",
}

// openaiRegistrar 协议注册器（持有单次注册的状态）
type openaiRegistrar struct {
	client       *http.Client
	sentinel     *sentinel.Solver
	profile      chromeProfile // 随机 Chrome 指纹
	acceptLang   string        // 随机 Accept-Language
	codeVerifier string
	state        string
	continueURL  string // create_account 返回的 continue_url（用于 OAuth 授权码提取）
	logf         func(string, ...interface{})
}

func newOpenAIRegistrar(proxy *ProxyEntry, logf func(string, ...interface{})) *openaiRegistrar {
	transport := &http.Transport{
		MaxIdleConns:    10,
		IdleConnTimeout: 30 * time.Second,
	}
	applyProxy(transport, proxy)

	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	client := &http.Client{
		Transport: transport,
		Jar:       jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 15 {
				return fmt.Errorf("超过最大重定向次数")
			}
			return nil
		},
		Timeout: 60 * time.Second,
	}

	sol := sentinel.New()
	// 设置 oai-did cookie（标准 cookie jar 通过 SetCookies 注入）
	authURL := mustParseURL(openaiAuthBase)
	jar.SetCookies(authURL, []*http.Cookie{{Name: "oai-did", Value: sol.DeviceID}})

	// 随机 Chrome 指纹和语言（与 codex.py _CHROME_PROFILES 多样性策略对齐）
	profile := chromeProfiles[rand.Intn(len(chromeProfiles))]
	acceptLang := acceptLanguages[rand.Intn(len(acceptLanguages))]

	return &openaiRegistrar{
		client:     client,
		sentinel:   sol,
		profile:    profile,
		acceptLang: acceptLang,
		logf:       logf,
	}
}

// commonHeaders 构造通用 API 请求头（使用随机 Chrome 指纹）
func (r *openaiRegistrar) commonHeaders(referer string) map[string]string {
	traceID := fmt.Sprintf("%d", rand.Int63())
	parentID := fmt.Sprintf("%d", rand.Int63())
	traceHex := fmt.Sprintf("%016x", rand.Int63())
	parentHex := fmt.Sprintf("%016x", rand.Int63())

	return map[string]string{
		"accept":                      "application/json",
		"accept-language":             r.acceptLang,
		"content-type":                "application/json",
		"origin":                      openaiAuthBase,
		"referer":                     referer,
		"user-agent":                  r.profile.ua,
		"oai-device-id":               r.sentinel.DeviceID,
		"sec-ch-ua":                   r.profile.secChUA,
		"sec-ch-ua-arch":              `"x86"`,
		"sec-ch-ua-bitness":           `"64"`,
		"sec-ch-ua-full-version-list": r.profile.secChUAFull,
		"sec-ch-ua-mobile":            "?0",
		"sec-ch-ua-platform":          `"Windows"`,
		"sec-ch-ua-platform-version":  `"` + r.profile.secChUAPlatVer + `"`,
		"sec-fetch-dest":              "empty",
		"sec-fetch-mode":              "cors",
		"sec-fetch-site":              "same-origin",
		"traceparent":                 fmt.Sprintf("00-0000000000000000%s-%s-01", traceHex, parentHex),
		"tracestate":                  "dd=s:1;o:rum",
		"x-datadog-origin":            "rum",
		"x-datadog-parent-id":         parentID,
		"x-datadog-sampling-priority": "1",
		"x-datadog-trace-id":          traceID,
	}
}

// navHeaders 页面导航请求头（使用随机 Chrome 指纹）
func (r *openaiRegistrar) navHeaders() map[string]string {
	return map[string]string{
		"accept":                      "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"accept-language":             r.acceptLang,
		"user-agent":                  r.profile.ua,
		"sec-ch-ua":                   r.profile.secChUA,
		"sec-ch-ua-arch":              `"x86"`,
		"sec-ch-ua-bitness":           `"64"`,
		"sec-ch-ua-full-version-list": r.profile.secChUAFull,
		"sec-ch-ua-mobile":            "?0",
		"sec-ch-ua-platform":          `"Windows"`,
		"sec-ch-ua-platform-version":  `"` + r.profile.secChUAPlatVer + `"`,
		"sec-fetch-dest":              "document",
		"sec-fetch-mode":              "navigate",
		"sec-fetch-site":              "same-origin",
		"sec-fetch-user":              "?1",
		"upgrade-insecure-requests":   "1",
	}
}

// doRequest 执行 HTTP 请求，设置 headers
func (r *openaiRegistrar) doRequest(ctx context.Context, method, rawURL string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp, respBody, nil
}

// generatePKCE 生成 PKCE code_verifier 和 code_challenge（S256）
func generatePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err = cryptorand.Read(b); err != nil {
		return
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	digest := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(digest[:])
	return
}

// step0 OAuth 会话初始化 + 邮箱提交
func (r *openaiRegistrar) step0(ctx context.Context, email string) error {
	r.logf("[*] 步骤0: OAuth 会话初始化")

	verifier, challenge, err := generatePKCE()
	if err != nil {
		return fmt.Errorf("生成 PKCE 失败: %w", err)
	}
	r.codeVerifier = verifier

	stateBytes := make([]byte, 32)
	if _, err := cryptorand.Read(stateBytes); err != nil {
		return fmt.Errorf("生成 state 失败: %w", err)
	}
	r.state = base64.RawURLEncoding.EncodeToString(stateBytes)

	// 步骤0a: GET /oauth/authorize → 获取 login_session cookie
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {oauthClientID},
		"redirect_uri":          {oauthRedirectURI},
		"scope":                 {"openid profile email offline_access"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {r.state},
		"screen_hint":           {"signup"},
		"prompt":                {"login"},
	}
	authorizeURL := openaiAuthBase + "/oauth/authorize?" + params.Encode()

	r.logf("[*] 步骤0a: GET /oauth/authorize")
	resp, _, err := r.doRequest(ctx, "GET", authorizeURL, nil, r.navHeaders())
	if err != nil {
		return fmt.Errorf("步骤0a 失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("步骤0a HTTP %d", resp.StatusCode)
	}

	// 检查 login_session cookie
	hasLoginSession := false
	for _, cookie := range r.client.Jar.Cookies(mustParseURL(openaiAuthBase)) {
		if cookie.Name == "login_session" {
			hasLoginSession = true
			break
		}
	}
	if !hasLoginSession {
		return fmt.Errorf("未获取到 login_session cookie")
	}
	r.logf("[*] login_session 已获取")

	// 步骤0b: POST /api/accounts/authorize/continue → 提交邮箱
	r.logf("[*] 步骤0b: 提交邮箱 + 获取 sentinel token")
	sentinelToken, err := r.sentinel.BuildToken(ctx, r.client, "authorize_continue")
	if err != nil {
		return fmt.Errorf("获取 sentinel token 失败: %w", err)
	}

	hdrs := r.commonHeaders(openaiAuthBase + "/create-account")
	hdrs["openai-sentinel-token"] = sentinelToken

	payload, _ := json.Marshal(map[string]interface{}{
		"username":    map[string]string{"kind": "email", "value": email},
		"screen_hint": "signup",
	})

	resp, body, err := r.doRequest(ctx, "POST", openaiAuthBase+"/api/accounts/authorize/continue", payload, hdrs)
	if err != nil {
		return fmt.Errorf("步骤0b 失败: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("步骤0b HTTP %d: %s", resp.StatusCode, truncStr(string(body), 200))
	}

	r.logf("[+] 步骤0 完成")
	return nil
}

// step2 注册用户（邮箱+密码）
func (r *openaiRegistrar) step2(ctx context.Context, email, password string) error {
	r.logf("[*] 步骤2: 注册用户（username+password）")

	sentinelToken, err := r.sentinel.BuildToken(ctx, r.client, "authorize_continue")
	if err != nil {
		return fmt.Errorf("获取 sentinel token 失败: %w", err)
	}

	hdrs := r.commonHeaders(openaiAuthBase + "/create-account/password")
	hdrs["openai-sentinel-token"] = sentinelToken

	// 注意：字段名是 username 而非 email（浏览器抓包验证）
	payload, _ := json.Marshal(map[string]string{
		"username": email,
		"password": password,
	})

	resp, body, err := r.doRequest(ctx, "POST", openaiAuthBase+"/api/accounts/user/register", payload, hdrs)
	if err != nil {
		return fmt.Errorf("步骤2 失败: %w", err)
	}

	if resp.StatusCode == 200 {
		r.logf("[+] 步骤2 注册成功")
		return nil
	}
	// 302 重定向到 email-otp 也算成功
	if resp.StatusCode == 302 || resp.StatusCode == 301 {
		loc := resp.Header.Get("Location")
		if strings.Contains(loc, "email-otp") || strings.Contains(loc, "email-verification") {
			r.logf("[+] 步骤2 注册成功（302 重定向）")
			return nil
		}
	}
	return fmt.Errorf("步骤2 HTTP %d: %s", resp.StatusCode, truncStr(string(body), 200))
}

// step3 触发验证码发送
func (r *openaiRegistrar) step3(ctx context.Context) error {
	r.logf("[*] 步骤3: 触发验证码发送")

	hdrs := r.navHeaders()
	hdrs["referer"] = openaiAuthBase + "/create-account/password"

	if resp, _, err := r.doRequest(ctx, "GET", openaiAuthBase+"/api/accounts/email-otp/send", nil, hdrs); err != nil {
		r.logf("[!] 步骤3 email-otp/send 请求失败: %s", err)
	} else if resp.StatusCode >= 400 {
		r.logf("[!] 步骤3 email-otp/send HTTP %d", resp.StatusCode)
	}
	r.doRequest(ctx, "GET", openaiAuthBase+"/email-verification", nil, hdrs) //nolint:errcheck

	r.logf("[+] 步骤3 完成")
	return nil
}

// step4 提交验证码
func (r *openaiRegistrar) step4(ctx context.Context, code string) error {
	r.logf("[*] 步骤4: 提交验证码 %s", code)

	hdrs := r.commonHeaders(openaiAuthBase + "/email-verification")
	payload, _ := json.Marshal(map[string]string{"code": code})

	resp, body, err := r.doRequest(ctx, "POST", openaiAuthBase+"/api/accounts/email-otp/validate", payload, hdrs)
	if err != nil {
		return fmt.Errorf("步骤4 失败: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("步骤4 HTTP %d: %s", resp.StatusCode, truncStr(string(body), 200))
	}

	r.logf("[+] 步骤4 验证码通过")
	return nil
}

// step5 提交姓名+生日完成注册
func (r *openaiRegistrar) step5(ctx context.Context, firstName, lastName, birthdate string) error {
	r.logf("[*] 步骤5: 创建账号（%s %s, %s）", firstName, lastName, birthdate)

	// 与 step0b/step2 保持一致：先获取 sentinel token 再发请求，避免 403 往返
	sentinelToken, err := r.sentinel.BuildToken(ctx, r.client, "authorize_continue")
	if err != nil {
		return fmt.Errorf("步骤5 获取 sentinel token 失败: %w", err)
	}

	hdrs := r.commonHeaders(openaiAuthBase + "/about-you")
	hdrs["openai-sentinel-token"] = sentinelToken

	payload, _ := json.Marshal(map[string]string{
		"name":      firstName + " " + lastName,
		"birthdate": birthdate,
	})

	resp, body, err := r.doRequest(ctx, "POST", openaiAuthBase+"/api/accounts/create_account", payload, hdrs)
	if err != nil {
		return fmt.Errorf("步骤5 失败: %w", err)
	}

	// 从响应中提取 continue_url（用于后续 consent 流程）
	parseContinueURL := func(data []byte) {
		var result map[string]interface{}
		if json.Unmarshal(data, &result) == nil {
			for _, key := range []string{"continue_url", "url", "redirect_url"} {
				if v, ok := result[key].(string); ok && v != "" {
					r.continueURL = v
					r.logf("[*] 步骤5 continue_url: %s", truncStr(v, 80))
					return
				}
			}
		}
	}

	if resp.StatusCode == 200 {
		parseContinueURL(body)
		r.logf("[+] 步骤5 账号创建完成")
		return nil
	}
	// 如果仍然 403 含 sentinel，用新 token 重试一次
	if resp.StatusCode == 403 && strings.Contains(strings.ToLower(string(body)), "sentinel") {
		r.logf("[*] 步骤5 sentinel 被拒，刷新 token 重试...")
		retryToken, stErr := r.sentinel.BuildToken(ctx, r.client, "authorize_continue")
		if stErr != nil {
			return fmt.Errorf("步骤5 sentinel 重试求解失败: %w", stErr)
		}
		hdrs["openai-sentinel-token"] = retryToken
		resp, body, err = r.doRequest(ctx, "POST", openaiAuthBase+"/api/accounts/create_account", payload, hdrs)
		if err == nil && resp.StatusCode == 200 {
			parseContinueURL(body)
			r.logf("[+] 步骤5 账号创建完成（sentinel 重试成功）")
			return nil
		}
	}
	if resp.StatusCode == 301 || resp.StatusCode == 302 {
		r.logf("[+] 步骤5 收到重定向，注册可能已完成")
		return nil
	}
	return fmt.Errorf("步骤5 HTTP %d: %s", resp.StatusCode, truncStr(string(body), 200))
}

// extractCodeFromURL 从 URL 中提取 authorization code
func extractCodeFromURL(rawURL string) string {
	if rawURL == "" || !strings.Contains(rawURL, "code=") {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("code")
}

func extractContinueURLAndPageType(body []byte) (continueURL, pageType string) {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", ""
	}
	for _, key := range []string{"continue_url", "url", "redirect_url"} {
		if value, ok := result[key].(string); ok && value != "" {
			continueURL = value
			break
		}
	}
	if page, ok := result["page"].(map[string]interface{}); ok {
		pageType, _ = page["type"].(string)
	}
	return continueURL, pageType
}

func oauthNeedsEmailOTP(continueURL, pageType string) bool {
	pageType = strings.ToLower(strings.TrimSpace(pageType))
	continueURL = strings.ToLower(strings.TrimSpace(continueURL))
	return pageType == "email_otp_verification" ||
		strings.Contains(continueURL, "email-verification") ||
		strings.Contains(continueURL, "email-otp")
}

func oauthNeedsConsentFallback(pageType string) bool {
	pageType = strings.ToLower(strings.TrimSpace(pageType))
	return strings.Contains(pageType, "consent") || strings.Contains(pageType, "organization")
}

func (r *openaiRegistrar) hasLoginSession() bool {
	for _, cookie := range r.client.Jar.Cookies(mustParseURL(openaiAuthBase)) {
		if cookie.Name == "login_session" {
			return true
		}
	}
	return false
}

func (r *openaiRegistrar) bootstrapOAuthSession(ctx context.Context, authorizeURL, fallbackURL string) (string, error) {
	resp, _, err := r.doRequest(ctx, "GET", authorizeURL, nil, r.navHeaders())
	if err != nil {
		return "", err
	}

	finalURL := authorizeURL
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if r.hasLoginSession() {
		return finalURL, nil
	}

	hdrs := r.navHeaders()
	hdrs["referer"] = authorizeURL
	resp, _, err = r.doRequest(ctx, "GET", fallbackURL, nil, hdrs)
	if err != nil {
		return finalURL, err
	}
	if resp != nil && resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if !r.hasLoginSession() {
		return finalURL, fmt.Errorf("未获取到 login_session cookie")
	}
	return finalURL, nil
}

// noRedirectClient 创建不跟随重定向的 HTTP 客户端（共享 transport 和 cookie jar）
func (r *openaiRegistrar) noRedirectClient() *http.Client {
	return &http.Client{
		Transport: r.client.Transport,
		Jar:       r.client.Jar,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// followRedirectsForCode 手动跟随重定向链，提取 authorization code
// localhost 连接失败时从错误信息中提取回调 URL
func (r *openaiRegistrar) followRedirectsForCode(ctx context.Context, startURL string, maxDepth int) string {
	nrc := r.noRedirectClient()
	current := startURL
	for i := 0; i < maxDepth; i++ {
		req, err := http.NewRequestWithContext(ctx, "GET", current, nil)
		if err != nil {
			return ""
		}
		for k, v := range r.navHeaders() {
			req.Header.Set(k, v)
		}
		resp, err := nrc.Do(req)
		if err != nil {
			// localhost 连接失败 → 从错误信息提取 code
			errStr := err.Error()
			if strings.Contains(errStr, "localhost") || strings.Contains(errStr, "127.0.0.1") {
				// 尝试从 URL 模式中提取
				for _, prefix := range []string{"http://localhost", "http://127.0.0.1"} {
					if idx := strings.Index(errStr, prefix); idx >= 0 {
						// 截取 URL 直到空格或引号
						sub := errStr[idx:]
						for _, end := range []string{" ", "'", "\"", ")"} {
							if ei := strings.Index(sub, end); ei > 0 {
								sub = sub[:ei]
								break
							}
						}
						if code := extractCodeFromURL(sub); code != "" {
							return code
						}
					}
				}
			}
			return ""
		}
		io.ReadAll(io.LimitReader(resp.Body, 1024)) //nolint:errcheck
		resp.Body.Close()

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			if code := extractCodeFromURL(loc); code != "" {
				return code
			}
			if loc == "" {
				return ""
			}
			// 相对路径 → 拼接完整 URL
			if strings.HasPrefix(loc, "/") {
				loc = openaiAuthBase + loc
			}
			current = loc
			continue
		}
		// 200 → 检查最终 URL
		if code := extractCodeFromURL(resp.Request.URL.String()); code != "" {
			return code
		}
		return ""
	}
	return ""
}

// decodeAuthSessionCookie 解码 oai-client-auth-session cookie（Flask/itsdangerous 格式）
// 格式: base64(json).timestamp.signature — 第一段 base64 解码后是 JSON
func (r *openaiRegistrar) decodeAuthSessionCookie() map[string]interface{} {
	cookies := r.client.Jar.Cookies(mustParseURL(openaiAuthBase))
	for _, c := range cookies {
		if c.Name != "oai-client-auth-session" {
			continue
		}
		val := c.Value
		firstPart := val
		if dotIdx := strings.Index(val, "."); dotIdx > 0 {
			firstPart = val[:dotIdx]
		}
		// 补齐 base64url padding
		switch len(firstPart) % 4 {
		case 2:
			firstPart += "=="
		case 3:
			firstPart += "="
		}
		decoded, err := base64.URLEncoding.DecodeString(firstPart)
		if err != nil {
			// 尝试 StdEncoding
			decoded, err = base64.StdEncoding.DecodeString(firstPart)
			if err != nil {
				continue
			}
		}
		var data map[string]interface{}
		if json.Unmarshal(decoded, &data) == nil {
			return data
		}
	}
	return nil
}

// step6 获取授权码（完整 consent 流程：workspace/select → organization/select → 提取 code）
func (r *openaiRegistrar) step6(ctx context.Context) (string, error) {
	r.logf("[*] 步骤6: 获取授权码（consent 流程）")

	// 确定 consent URL
	consentURL := r.continueURL

	if consentURL == "" {
		consentURL = openaiConsentURL
	} else if strings.HasPrefix(consentURL, "/") {
		consentURL = openaiAuthBase + consentURL
	}

	// OpenAI 新增手机验证/邮箱验证拦截页 → 跳过，直接走默认 consent 路径
	normalizedConsentURL := strings.ToLower(consentURL)
	if strings.Contains(normalizedConsentURL, "/add-phone") || strings.Contains(normalizedConsentURL, "/email-verification") {
		r.logf("[*] 步骤6: 检测到拦截页 (%s)，跳过直接走 consent", truncStr(consentURL, 60))
		consentURL = openaiConsentURL
	}

	r.logf("[*] consent URL: %s", truncStr(consentURL, 100))

	nrc := r.noRedirectClient()

	// ── 6a: GET consent 页面（触发 cookie 设置） ──
	r.logf("[*] 步骤6a: GET consent 页面")
	req, err := http.NewRequestWithContext(ctx, "GET", consentURL, nil)
	if err != nil {
		return "", fmt.Errorf("步骤6a 请求创建失败: %w", err)
	}
	for k, v := range r.navHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := nrc.Do(req)
	if err != nil {
		// localhost 连接失败 → 可能已被直接重定向到回调
		errStr := err.Error()
		if strings.Contains(errStr, "localhost") || strings.Contains(errStr, "127.0.0.1") {
			if code := r.followRedirectsForCode(ctx, consentURL, 1); code != "" {
				r.logf("[+] 步骤6a consent ConnectionError 中提取到授权码")
				return code, nil
			}
		}
		return "", fmt.Errorf("步骤6a 请求失败: %w", err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()

	// 直接 302 带 code
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if code := extractCodeFromURL(loc); code != "" {
			r.logf("[+] 步骤6a consent 直接 302 获取到授权码")
			return code, nil
		}
		// 跟踪重定向链
		if loc != "" {
			if strings.HasPrefix(loc, "/") {
				loc = openaiAuthBase + loc
			}
			if code := r.followRedirectsForCode(ctx, loc, 10); code != "" {
				r.logf("[+] 步骤6a 跟踪重定向获取到授权码")
				return code, nil
			}
		}
	}
	_ = respBody // consent 页面 HTML，不需要解析

	// ── 6b: 解码 session cookie → 提取 workspace_id → POST workspace/select ──
	r.logf("[*] 步骤6b: 解码 session cookie → workspace/select")
	sessionData := r.decodeAuthSessionCookie()
	var workspaceID string
	if sessionData != nil {
		if workspaces, ok := sessionData["workspaces"].([]interface{}); ok && len(workspaces) > 0 {
			if ws, ok := workspaces[0].(map[string]interface{}); ok {
				if id, ok := ws["id"].(string); ok {
					workspaceID = id
				}
			}
		}
	}

	if workspaceID != "" {
		r.logf("[*] workspace_id: %s", workspaceID)

		hdrs := r.commonHeaders(consentURL)
		payload, _ := json.Marshal(map[string]string{"workspace_id": workspaceID})

		req, _ := http.NewRequestWithContext(ctx, "POST",
			openaiAuthBase+"/api/accounts/workspace/select",
			bytes.NewReader(payload))
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}

		resp, err := nrc.Do(req)
		if err == nil {
			wsBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()

			// 302 → 直接提取 code
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				loc := resp.Header.Get("Location")
				if code := extractCodeFromURL(loc); code != "" {
					r.logf("[+] workspace/select 302 获取到授权码")
					return code, nil
				}
				if loc != "" {
					if strings.HasPrefix(loc, "/") {
						loc = openaiAuthBase + loc
					}
					if code := r.followRedirectsForCode(ctx, loc, 10); code != "" {
						r.logf("[+] workspace/select 跟踪重定向获取到授权码")
						return code, nil
					}
				}
			}

			// 200 → 解析 JSON 进入 organization/select
			if resp.StatusCode == 200 {
				var wsResult map[string]interface{}
				if json.Unmarshal(wsBody, &wsResult) == nil {
					wsNext, _ := wsResult["continue_url"].(string)

					// ── 6c: organization/select ──
					orgID, projectID := r.extractOrgAndProject(wsResult)
					if orgID != "" {
						r.logf("[*] 步骤6c: POST organization/select (org=%s)", orgID)
						orgBody := map[string]string{"org_id": orgID}
						if projectID != "" {
							orgBody["project_id"] = projectID
						}
						orgPayload, _ := json.Marshal(orgBody)

						referer := consentURL
						if wsNext != "" {
							if strings.HasPrefix(wsNext, "/") {
								referer = openaiAuthBase + wsNext
							} else {
								referer = wsNext
							}
						}

						orgHdrs := r.commonHeaders(referer)
						orgReq, _ := http.NewRequestWithContext(ctx, "POST",
							openaiAuthBase+"/api/accounts/organization/select",
							bytes.NewReader(orgPayload))
						for k, v := range orgHdrs {
							orgReq.Header.Set(k, v)
						}

						orgResp, orgErr := nrc.Do(orgReq)
						if orgErr == nil {
							orgRespBody, _ := io.ReadAll(io.LimitReader(orgResp.Body, 64*1024))
							orgResp.Body.Close()

							if orgResp.StatusCode >= 300 && orgResp.StatusCode < 400 {
								loc := orgResp.Header.Get("Location")
								if code := extractCodeFromURL(loc); code != "" {
									r.logf("[+] organization/select 302 获取到授权码")
									return code, nil
								}
								if loc != "" {
									if strings.HasPrefix(loc, "/") {
										loc = openaiAuthBase + loc
									}
									if code := r.followRedirectsForCode(ctx, loc, 10); code != "" {
										r.logf("[+] organization/select 跟踪重定向获取到授权码")
										return code, nil
									}
								}
							}
							if orgResp.StatusCode == 200 {
								var orgResult map[string]interface{}
								if json.Unmarshal(orgRespBody, &orgResult) == nil {
									if orgNext, ok := orgResult["continue_url"].(string); ok && orgNext != "" {
										if strings.HasPrefix(orgNext, "/") {
											orgNext = openaiAuthBase + orgNext
										}
										if code := r.followRedirectsForCode(ctx, orgNext, 10); code != "" {
											r.logf("[+] organization/select continue_url 跟踪获取到授权码")
											return code, nil
										}
									}
								}
							}
						}
					}

					// workspace/select 返回非 organization 的 continue_url → 直接跟踪
					if wsNext != "" {
						if strings.HasPrefix(wsNext, "/") {
							wsNext = openaiAuthBase + wsNext
						}
						if code := r.followRedirectsForCode(ctx, wsNext, 10); code != "" {
							r.logf("[+] workspace/select continue_url 跟踪获取到授权码")
							return code, nil
						}
					}
				}
			}
		}
	} else {
		r.logf("[*] 未找到 workspace_id，跳过 workspace/select")
	}

	// ── 6d: 备用策略 — 跟随重定向（允许跟踪）捕获 code ──
	r.logf("[*] 步骤6d: 备用策略 — 完整重定向跟踪")
	if code := r.followRedirectsForCode(ctx, consentURL, 15); code != "" {
		r.logf("[+] 步骤6d 备用策略获取到授权码")
		return code, nil
	}

	// ── 6e: POST authorize/continue 最终尝试 ──
	r.logf("[*] 步骤6e: POST authorize/continue 最终尝试")
	acHdrs := r.commonHeaders(openaiAuthBase + "/about-you")
	sentinelToken, stErr := r.sentinel.BuildToken(ctx, r.client, "authorize_continue")
	if stErr != nil {
		r.logf("[!] 步骤6e sentinel 求解失败: %s", stErr)
	}
	if sentinelToken != "" {
		acHdrs["openai-sentinel-token"] = sentinelToken
	}
	acPayload, _ := json.Marshal(map[string]interface{}{})

	acReq, _ := http.NewRequestWithContext(ctx, "POST",
		openaiAuthBase+"/api/accounts/authorize/continue",
		bytes.NewReader(acPayload))
	for k, v := range acHdrs {
		acReq.Header.Set(k, v)
	}
	acResp, acErr := nrc.Do(acReq)
	if acErr == nil {
		acBody, _ := io.ReadAll(io.LimitReader(acResp.Body, 64*1024))
		acResp.Body.Close()

		// 从 JSON 提取 redirect_url
		var acResult map[string]interface{}
		if json.Unmarshal(acBody, &acResult) == nil {
			for _, key := range []string{"redirect_url", "continue_url", "url"} {
				if v, ok := acResult[key].(string); ok && v != "" {
					if code := extractCodeFromURL(v); code != "" {
						r.logf("[+] 步骤6e authorize/continue 获取到授权码")
						return code, nil
					}
					// 跟踪 URL
					if strings.HasPrefix(v, "/") {
						v = openaiAuthBase + v
					}
					if code := r.followRedirectsForCode(ctx, v, 10); code != "" {
						r.logf("[+] 步骤6e 跟踪获取到授权码")
						return code, nil
					}
				}
			}
		}
		// 从 Location 提取
		if loc := acResp.Header.Get("Location"); loc != "" {
			if code := extractCodeFromURL(loc); code != "" {
				r.logf("[+] 步骤6e Location 获取到授权码")
				return code, nil
			}
		}
	}

	return "", fmt.Errorf("步骤6 consent 流程未获取到授权码")
}

// extractOrgAndProject 从 workspace/select 响应中提取 org_id 和 project_id
func (r *openaiRegistrar) extractOrgAndProject(wsResult map[string]interface{}) (orgID, projectID string) {
	data, ok := wsResult["data"].(map[string]interface{})
	if !ok {
		return
	}
	orgs, ok := data["orgs"].([]interface{})
	if !ok || len(orgs) == 0 {
		return
	}
	firstOrg, ok := orgs[0].(map[string]interface{})
	if !ok {
		return
	}
	orgID, _ = firstOrg["id"].(string)
	if projects, ok := firstOrg["projects"].([]interface{}); ok && len(projects) > 0 {
		if proj, ok := projects[0].(map[string]interface{}); ok {
			projectID, _ = proj["id"].(string)
		}
	}
	return
}

// oauthLogin 全流程 OAuth 重新登录（独立 session，用于注册后获取 token 的 fallback）
// 流程: authorize → email → password/verify → [email-otp if needed] → consent → code → token exchange
func (r *openaiRegistrar) oauthLogin(ctx context.Context, email, password string, mp *tempmail.MultiProvider, mailMeta map[string]string) (map[string]interface{}, error) {
	r.logf("[*] OAuth 重新登录: 创建新 session")

	// 创建独立的 registrar（新 session、新 PKCE、新 device_id）
	login := newOpenAIRegistrar(nil, r.logf)
	// 复用原始 registrar 的 transport（继承代理设置）
	login.client.Transport = r.client.Transport

	// 步骤L1: GET /oauth/authorize（获取 login_session）
	r.logf("[*] OAuth 登录步骤1: GET /oauth/authorize")
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return nil, fmt.Errorf("PKCE 生成失败: %w", err)
	}
	login.codeVerifier = verifier

	stateBytes := make([]byte, 32)
	cryptorand.Read(stateBytes) //nolint:errcheck
	login.state = base64.RawURLEncoding.EncodeToString(stateBytes)

	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {oauthClientID},
		"redirect_uri":          {oauthRedirectURI},
		"scope":                 {"openid profile email offline_access"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {login.state},
	}
	authorizeURL := openaiAuthBase + "/oauth/authorize?" + params.Encode()
	bootstrapURL := openaiAuthBase + "/api/oauth/oauth2/auth?" + params.Encode()
	authorizeFinalURL, err := login.bootstrapOAuthSession(ctx, authorizeURL, bootstrapURL)
	if err != nil {
		return nil, fmt.Errorf("OAuth 登录步骤1 失败: %w", err)
	}

	// 步骤L2: POST authorize/continue（提交邮箱）
	r.logf("[*] OAuth 登录步骤2: 提交邮箱")
	sentinelEmail, err := login.sentinel.BuildToken(ctx, login.client, "authorize_continue")
	if err != nil {
		return nil, fmt.Errorf("OAuth 登录 sentinel(email) 失败: %w", err)
	}
	hdrs := login.commonHeaders(authorizeFinalURL)
	hdrs["openai-sentinel-token"] = sentinelEmail

	emailPayload, _ := json.Marshal(map[string]interface{}{
		"username": map[string]string{"kind": "email", "value": email},
	})
	resp, body, err := login.doRequest(ctx, "POST",
		openaiAuthBase+"/api/accounts/authorize/continue", emailPayload, hdrs)
	if err != nil {
		return nil, fmt.Errorf("OAuth 登录步骤2 失败: %w", err)
	}
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(body)), "invalid_auth_step") {
		r.logf("[*] OAuth 登录步骤2 命中 invalid_auth_step，重新 bootstrap 后重试")
		authorizeFinalURL, err = login.bootstrapOAuthSession(ctx, authorizeURL, bootstrapURL)
		if err != nil {
			return nil, fmt.Errorf("OAuth 登录步骤2 重置会话失败: %w", err)
		}
		sentinelEmail, err = login.sentinel.BuildToken(ctx, login.client, "authorize_continue")
		if err != nil {
			return nil, fmt.Errorf("OAuth 登录步骤2 重试 sentinel(email) 失败: %w", err)
		}
		hdrs = login.commonHeaders(authorizeFinalURL)
		hdrs["openai-sentinel-token"] = sentinelEmail
		resp, body, err = login.doRequest(ctx, "POST",
			openaiAuthBase+"/api/accounts/authorize/continue", emailPayload, hdrs)
		if err != nil {
			return nil, fmt.Errorf("OAuth 登录步骤2 重试失败: %w", err)
		}
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("OAuth 登录步骤2 HTTP %d: %s", resp.StatusCode, truncStr(string(body), 200))
	}
	if continueURL, pageType := extractContinueURLAndPageType(body); continueURL != "" || pageType != "" {
		if continueURL != "" {
			login.continueURL = continueURL
		}
		if pageType != "" {
			r.logf("[*] OAuth 登录邮箱阶段 page.type: %s", pageType)
		}
	}

	// 步骤L3: POST password/verify（提交密码）
	r.logf("[*] OAuth 登录步骤3: 提交密码")
	sentinelPwd, err := login.sentinel.BuildToken(ctx, login.client, "password_verify")
	if err != nil {
		return nil, fmt.Errorf("OAuth 登录 sentinel(pwd) 失败: %w", err)
	}
	pwdHdrs := login.commonHeaders(openaiAuthBase + "/log-in/password")
	pwdHdrs["openai-sentinel-token"] = sentinelPwd

	pwdPayload, _ := json.Marshal(map[string]string{"password": password})
	resp, body, err = login.doRequest(ctx, "POST",
		openaiAuthBase+"/api/accounts/password/verify", pwdPayload, pwdHdrs)
	if err != nil {
		return nil, fmt.Errorf("OAuth 登录步骤3 失败: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("OAuth 登录步骤3 HTTP %d: %s", resp.StatusCode, truncStr(string(body), 200))
	}

	// 解析 continue_url
	pwdContinue, pwdPageType := extractContinueURLAndPageType(body)
	if pwdContinue != "" {
		login.continueURL = pwdContinue
	}
	if pwdPageType != "" {
		r.logf("[*] OAuth 登录密码阶段 page.type: %s", pwdPageType)
	}
	if login.continueURL == "" && oauthNeedsConsentFallback(pwdPageType) {
		login.continueURL = openaiConsentURL
		r.logf("[*] OAuth 登录密码阶段未返回 continue_url，按 page.type 回退到 consent")
	}
	if login.continueURL == "" && !oauthNeedsEmailOTP(login.continueURL, pwdPageType) {
		return nil, fmt.Errorf("OAuth 登录步骤3 未获取到 continue_url")
	}
	if login.continueURL != "" {
		r.logf("[*] OAuth 登录 continue_url: %s", truncStr(login.continueURL, 80))
	}

	// 步骤L3b: 如果 continue_url 包含 email-verification，需要先完成邮箱 OTP 验证
	if oauthNeedsEmailOTP(login.continueURL, pwdPageType) {
		r.logf("[*] OAuth 登录: 检测到邮箱 OTP 验证要求，触发验证码...")

		// 触发 OTP 发送
		otpHdrs := login.navHeaders()
		otpHdrs["referer"] = openaiAuthBase + "/email-verification"
		login.doRequest(ctx, "GET", openaiAuthBase+"/api/accounts/email-otp/send", nil, otpHdrs) //nolint:errcheck
		login.doRequest(ctx, "GET", openaiAuthBase+"/email-verification", nil, otpHdrs)          //nolint:errcheck

		// 轮询获取验证码
		if mp == nil {
			return nil, fmt.Errorf("OAuth 登录需要邮箱 OTP 但 mail provider 不可用")
		}
		otpCode := pollMailCode(ctx, mp, email, mailMeta, 120*time.Second, r.logf)
		if otpCode == "" {
			return nil, fmt.Errorf("OAuth 登录邮箱 OTP 超时")
		}
		r.logf("[*] OAuth 登录: 提交 OTP %s", otpCode)

		// 提交验证码
		validateHdrs := login.commonHeaders(openaiAuthBase + "/email-verification")
		validatePayload, _ := json.Marshal(map[string]string{"code": otpCode})
		resp, body, err = login.doRequest(ctx, "POST",
			openaiAuthBase+"/api/accounts/email-otp/validate", validatePayload, validateHdrs)
		if err != nil {
			return nil, fmt.Errorf("OAuth 登录 OTP 验证失败: %w", err)
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("OAuth 登录 OTP 验证 HTTP %d: %s", resp.StatusCode, truncStr(string(body), 200))
		}

		otpContinue, otpPageType := extractContinueURLAndPageType(body)
		if otpContinue != "" {
			login.continueURL = otpContinue
			r.logf("[*] OAuth 登录 OTP 后 continue_url: %s", truncStr(otpContinue, 80))
		}
		if login.continueURL == "" && oauthNeedsConsentFallback(otpPageType) {
			login.continueURL = openaiConsentURL
			r.logf("[*] OAuth 登录 OTP 后未返回 continue_url，按 page.type 回退到 consent")
		}
	}

	// 步骤L4: consent 流程提取 code（复用 step6 的 consent 逻辑）
	authCode, err := login.step6(ctx)
	if err != nil {
		return nil, fmt.Errorf("OAuth 登录 consent 失败: %w", err)
	}

	// 步骤L5: 换取 token
	codex, err := login.step7(ctx, authCode)
	if err != nil {
		return nil, fmt.Errorf("OAuth 登录 token 交换失败: %w", err)
	}

	return codex, nil
}

// step7 换取 access_token + id_token（PKCE token exchange）
func (r *openaiRegistrar) step7(ctx context.Context, authCode string) (map[string]interface{}, error) {
	r.logf("[*] 步骤7: 换取 token")

	formData := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {oauthRedirectURI},
		"code_verifier": {r.codeVerifier},
		"client_id":     {oauthClientID},
	}

	hdrs := map[string]string{
		"content-type": "application/x-www-form-urlencoded",
		"user-agent":   r.profile.ua,
		"accept":       "application/json",
		"origin":       openaiAuthBase,
	}

	resp, body, err := r.doRequest(ctx, "POST", openaiAuthBase+"/oauth/token",
		[]byte(formData.Encode()), hdrs)
	if err != nil {
		return nil, fmt.Errorf("步骤7 请求失败: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("步骤7 HTTP %d: %s", resp.StatusCode, truncStr(string(body), 200))
	}

	var tokenResp map[string]interface{}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("步骤7 响应解析失败: %w", err)
	}

	accessToken, _ := tokenResp["access_token"].(string)
	idToken, _ := tokenResp["id_token"].(string)
	if accessToken == "" {
		return nil, fmt.Errorf("步骤7 响应中无 access_token: %s", truncStr(string(body), 200))
	}

	// 解码 JWT payload
	claims, err := decodeJWTPayload(accessToken)
	if err != nil {
		return nil, fmt.Errorf("步骤7 %w", err)
	}

	// 组装 codex 格式 JSON
	codex := buildCodexJSON(accessToken, idToken, claims)
	r.logf("[+] 步骤7 token 获取成功")
	return codex, nil
}

// decodeJWTPayload 解码 JWT 的 payload 部分（不验证签名，仅提取 claims）
func decodeJWTPayload(token string) (map[string]interface{}, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("JWT 格式无效（期望 3 段，实际 %d 段）", len(parts))
	}

	// base64url 解码 payload（补齐 padding）
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("JWT payload 解码失败: %w", err)
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil, fmt.Errorf("JWT claims 解析失败: %w", err)
	}

	return claims, nil
}

// buildCodexJSON 组装完整的 codex 格式 JSON（与 codex.py 输出对齐）
func buildCodexJSON(accessToken, idToken string, claims map[string]interface{}) map[string]interface{} {
	codex := make(map[string]interface{})

	// 复制所有 JWT claims（aud, sub, iss, exp, iat, nbf, scp, jti, session_id 等）
	for k, v := range claims {
		codex[k] = v
	}

	// 添加 token 字段
	codex["access_token"] = accessToken
	if idToken != "" {
		codex["id_token"] = idToken
	}
	codex["type"] = "codex"
	codex["disabled"] = false
	codex["client_id"] = oauthClientID

	// 格式化 expired 时间（ISO 8601）
	if exp, ok := claims["exp"].(float64); ok {
		t := time.Unix(int64(exp), 0).UTC()
		codex["expired"] = t.Format("2006-01-02T15:04:05+00:00")
	}

	// 添加 last_refresh 时间戳
	codex["last_refresh"] = time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")

	// 从嵌套 claims 提取 account_id
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]interface{}); ok {
		if accountID, ok := auth["chatgpt_account_id"].(string); ok {
			codex["account_id"] = accountID
		}
	}

	return codex
}

// 随机姓名池
var (
	openaiFirstNames = []string{
		"James", "Robert", "John", "Michael", "David", "William", "Richard",
		"Mary", "Jennifer", "Linda", "Elizabeth", "Susan", "Jessica", "Sarah",
		"Emily", "Emma", "Olivia", "Sophia", "Liam", "Noah", "Oliver", "Ethan",
	}
	openaiLastNames = []string{
		"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller",
		"Davis", "Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Martin",
	}
)

func randomOpenAIName() (string, string) {
	return openaiFirstNames[rand.Intn(len(openaiFirstNames))],
		openaiLastNames[rand.Intn(len(openaiLastNames))]
}

func randomBirthdate() string {
	year := 1985 + rand.Intn(18) // 1985-2002
	month := 1 + rand.Intn(12)
	day := 1 + rand.Intn(28)
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day)
}

// registerViaProtocol 纯 HTTP 协议注册主流程
func (w *OpenAIWorker) registerViaProtocol(ctx context.Context, opts RegisterOpts, logf func(string, ...interface{})) {
	// 平台专属邮箱 provider 覆盖全局优先级
	if platformProviders := opts.Config["openai_email_providers"]; platformProviders != "" {
		opts.Config["email_provider_priority"] = platformProviders
	}
	mailProvider := tempmail.NewMultiProvider(opts.Config)

	// 1. 创建临时邮箱（多 provider 自动切换）
	email, mailMeta, err := mailProvider.GenerateEmail(ctx)
	if err != nil {
		fmt.Printf("[DEBUG] 创建邮箱失败: %s\n", err)
		opts.OnFail()
		return
	}
	password := genOpenAIPassword(16)
	logf("[*] 邮箱: %s (via %s)", email, mailMeta["provider"])

	cleanup := func() { go mailProvider.DeleteEmail(context.Background(), email, mailMeta) }

	reg := newOpenAIRegistrar(opts.Proxy, logf)
	firstName, lastName := randomOpenAIName()
	birthdate := randomBirthdate()

	// 预热：访问 chatgpt.com 首页建立 session，与 Python 版 visit_homepage() 对齐
	logf("[*] 预热中...")
	reg.doRequest(ctx, "GET", "https://chatgpt.com/", nil, reg.navHeaders()) //nolint:errcheck
	ctxSleep(ctx, 300*time.Millisecond)

	// 步骤0
	if err := reg.step0(ctx, email); err != nil {
		fmt.Printf("[DEBUG] %s step0: %s\n", email, err)
		cleanup()
		opts.OnFail()
		return
	}
	ctxSleep(ctx, time.Second)

	// 步骤2
	if err := reg.step2(ctx, email, password); err != nil {
		fmt.Printf("[DEBUG] %s step2: %s\n", email, err)
		cleanup()
		opts.OnFail()
		return
	}
	ctxSleep(ctx, time.Second)

	// 步骤3
	if err := reg.step3(ctx); err != nil {
		fmt.Printf("[DEBUG] %s step3 发送验证码可能失败: %s（继续等待）\n", email, err)
	}

	// 等待验证码（30s 超时，超时后尝试换邮箱内联重试一次）
	logf("[*] 等待验证码...")
	code := pollMailCode(ctx, mailProvider, email, mailMeta, 30*time.Second, logf)
	if code == "" {
		fmt.Printf("[DEBUG] %s 验证码超时，尝试换邮箱重试\n", email)
		tempmail.RecordFailure(mailMeta["provider"])
		cleanup()

		// 内联重试：用同一个 MultiProvider 换邮箱（会自动降级到下一个 provider）
		email2, mailMeta2, err2 := mailProvider.GenerateEmail(ctx)
		if err2 != nil {
			fmt.Printf("[DEBUG] 换邮箱失败: %s\n", err2)
			opts.OnFail()
			return
		}
		logf("[*] 换邮箱重试: %s (via %s)", email2, mailMeta2["provider"])
		cleanup2 := func() { go mailProvider.DeleteEmail(context.Background(), email2, mailMeta2) }

		// 用新邮箱重走 step0-3
		reg2 := newOpenAIRegistrar(opts.Proxy, logf)
		if err := reg2.step0(ctx, email2); err != nil {
			fmt.Printf("[DEBUG] %s 重试 step0: %s\n", email2, err)
			cleanup2()
			opts.OnFail()
			return
		}
		if err := reg2.step2(ctx, email2, password); err != nil {
			fmt.Printf("[DEBUG] %s 重试 step2: %s\n", email2, err)
			cleanup2()
			opts.OnFail()
			return
		}
		if err := reg2.step3(ctx); err != nil {
			fmt.Printf("[DEBUG] %s 重试 step3 可能失败: %s\n", email2, err)
		}
		code = pollMailCode(ctx, mailProvider, email2, mailMeta2, 30*time.Second, logf)
		if code == "" {
			fmt.Printf("[DEBUG] %s 换邮箱后验证码仍超时\n", email2)
			tempmail.RecordFailure(mailMeta2["provider"])
			cleanup2()
			opts.OnFail()
			return
		}
		tempmail.RecordSuccess(mailMeta2["provider"])
		// 切换到新邮箱继续后续流程
		email = email2
		mailMeta = mailMeta2
		cleanup = cleanup2
		reg = reg2
		logf("[*] 验证码: %s", code)
	} else {
		logf("[*] 验证码: %s", code)
		tempmail.RecordSuccess(mailMeta["provider"])
	}

	// 步骤4（失败时重发验证码重试一次，与 Python DuckMail 版逻辑对齐）
	if err := reg.step4(ctx, code); err != nil {
		fmt.Printf("[DEBUG] %s step4: %s，重发验证码重试\n", email, err)
		if err := reg.step3(ctx); err != nil {
			fmt.Printf("[DEBUG] %s 重发 step3 可能失败: %s\n", email, err)
		}
		retryCode := pollMailCode(ctx, mailProvider, email, mailMeta, 60*time.Second, logf)
		if retryCode == "" || retryCode == code {
			fmt.Printf("[DEBUG] %s 重试验证码超时或未刷新\n", email)
			cleanup()
			opts.OnFail()
			return
		}
		logf("[*] 重试验证码: %s", retryCode)
		if err2 := reg.step4(ctx, retryCode); err2 != nil {
			fmt.Printf("[DEBUG] %s step4 重试: %s\n", email, err2)
			cleanup()
			opts.OnFail()
			return
		}
	}
	ctxSleep(ctx, time.Second)

	// 步骤5（只是填姓名生日，此时账号已注册成功，失败不删邮箱）
	step5Failed := false
	if err := reg.step5(ctx, firstName, lastName, birthdate); err != nil {
		fmt.Printf("[DEBUG] %s step5 失败: %s（账号已创建，继续尝试获取 token）\n", email, err)
		step5Failed = true
	}

	if step5Failed {
		logf("[+] 注册部分成功: %s", email)
	} else {
		logf("[+] 注册成功: %s", email)
	}

	// ── OAuth Token 获取（多级降级 + 重试） ──
	logf("[*] 获取 Token...")

	// 第一级：从注册 session 的 consent 流程获取 code（step5 失败则跳过，session 状态不可靠）
	// OAuth 重试详情不推送前端，仅在服务端 stdout 输出
	if !step5Failed {
		// 等待 3-6s（随机抖动），让 OpenAI 后端状态充分传播
		jitter := time.Duration(3000+rand.Intn(3000)) * time.Millisecond
		ctxSleep(ctx, jitter)
		authCode, err := reg.step6(ctx)
		if err == nil {
			codex, tokenErr := reg.step7(ctx, authCode)
			if tokenErr == nil {
				logf("[+] Token 获取成功")
				codex["email"] = email
				codex["password"] = password
				opts.OnSuccess(email, codex)
				return
			}
			fmt.Printf("[DEBUG] %s Token 交换失败: %s\n", email, tokenErr)
		} else {
			fmt.Printf("[DEBUG] %s consent 流程失败: %s\n", email, err)
		}
	} else {
		fmt.Printf("[DEBUG] %s step5 失败，跳过 consent\n", email)
	}

	// 第二级：全流程 OAuth 重新登录（最多重试 3 次，递增延迟 + 随机抖动避免并发踩踏 429）
	oauthBaseDelays := []time.Duration{5 * time.Second, 12 * time.Second, 25 * time.Second}
	for attempt, base := range oauthBaseDelays {
		if ctx.Err() != nil {
			break
		}
		// 在基础延迟上加 0-3s 随机抖动，错开并发 worker
		jitter := time.Duration(rand.Intn(3000)) * time.Millisecond
		delay := base + jitter
		fmt.Printf("[DEBUG] %s OAuth 重新登录第 %d 次尝试，等待 %s...\n", email, attempt+1, delay)
		ctxSleep(ctx, delay)
		if ctx.Err() != nil {
			break
		}
		codex, loginErr := reg.oauthLogin(ctx, email, password, mailProvider, mailMeta)
		if loginErr == nil {
			logf("[+] Token 获取成功")
			codex["email"] = email
			codex["password"] = password
			opts.OnSuccess(email, codex)
			return
		}
		fmt.Printf("[DEBUG] %s OAuth 重新登录第 %d 次失败: %s\n", email, attempt+1, loginErr)
	}

	// 最终降级：Token 获取失败，视为失败（不保存不完整凭证）
	fmt.Printf("[DEBUG] %s Token 获取失败，标记为失败\n", email)
	opts.OnFail()
}

// pollMailCode 轮询获取验证码（多 provider 版本）
func pollMailCode(ctx context.Context, mp *tempmail.MultiProvider, email string, meta map[string]string, timeout time.Duration, logf func(string, ...interface{})) string {
	deadline := time.Now().Add(timeout)
	attempt := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(3 * time.Second):
		}
		attempt++
		code, err := mp.FetchVerificationCode(ctx, email, meta, 1, 0)
		if err == nil && code != "" {
			return code
		}
		if attempt%5 == 0 {
			remaining := time.Until(deadline).Round(time.Second)
			logf("[*] 等待验证码中... 已等待 %ds，剩余 %s", attempt*3, remaining)
		}
	}
	return ""
}

// ─── 辅助 ───

func mustParseURL(rawURL string) *url.URL {
	u, _ := url.Parse(rawURL)
	return u
}

// ─── 容器化服务调用 ───

type openaiServiceRequest struct {
	Email         string `json:"email"`
	Password      string `json:"password"`
	Proxy         string `json:"proxy,omitempty"`
	YYDSMailURL   string `json:"yydsmail_url,omitempty"`
	YYDSMailKey   string `json:"yydsmail_key,omitempty"`
	EmailPriority string `json:"email_priority,omitempty"` // 邮箱 Provider 优先级
}

type openaiServiceResponse struct {
	OK           bool     `json:"ok"`
	Email        string   `json:"email"`
	AccessToken  string   `json:"access_token"`
	SessionToken string   `json:"session_token"`
	UserID       string   `json:"user_id"`
	FirstName    string   `json:"first_name"`
	LastName     string   `json:"last_name"`
	Error        string   `json:"error"`
	Logs         []string `json:"logs"`
}

func (w *OpenAIWorker) registerViaService(ctx context.Context, serviceURL string, opts RegisterOpts, logf func(string, ...interface{})) (map[string]interface{}, bool) {
	// 配置诊断仅输出到 stdout，不推送前端
	fmt.Println("[DEBUG] 远程注册服务连接中...")

	proxyStr := ""
	if userProxy := opts.Config["user_proxy"]; userProxy != "" {
		proxyStr = userProxy
		fmt.Println("[DEBUG] 使用用户指定代理")
	} else if opts.Proxy != nil {
		if opts.Proxy.HTTPS != "" {
			proxyStr = opts.Proxy.HTTPS
		} else if opts.Proxy.HTTP != "" {
			proxyStr = opts.Proxy.HTTP
		}
	}
	// 容器内 127.0.0.1 指向自身，需替换为宿主机地址
	proxyStr = strings.ReplaceAll(proxyStr, "127.0.0.1", "host.docker.internal")
	proxyStr = strings.ReplaceAll(proxyStr, "localhost", "host.docker.internal")

	// 不在主后端创建邮箱，由远程 MultiProvider 处理
	// 优先使用平台级邮箱选择，为空时回退到全局优先级
	emailPriority := opts.Config["openai_email_providers"]
	if emailPriority == "" {
		emailPriority = settingOrDefault(opts.Config, "email_provider_priority", "yydsmail")
	}
	epParts := strings.Split(emailPriority, ",")
	for i := range epParts {
		epParts[i] = strings.TrimSpace(epParts[i])
	}
	tempmail.WeightedShuffleNames(epParts)

	// 诊断日志：发送到远程的配置（仅 stdout）
	fmt.Printf("[DEBUG] 邮箱优先级: %s\n", strings.Join(epParts, ","))
	if opts.Config["yydsmail_api_key"] != "" {
		fmt.Println("[DEBUG] yydsmail 凭证: 已配置")
	}

	reqBody := openaiServiceRequest{
		Proxy:         proxyStr,
		YYDSMailURL:   settingOrDefault(opts.Config, "yydsmail_base_url", "https://maliapi.215.im"),
		YYDSMailKey:   opts.Config["yydsmail_api_key"],
		EmailPriority: strings.Join(epParts, ","),
	}
	bodyJSON, _ := json.Marshal(reqBody)

	// 关联任务 ctx，停止任务时立即取消外部服务请求
	// 600s 超时：排队等待（最多 ~300s）+ 实际注册（60-80s）+ 余量
	reqCtx, reqCancel := context.WithTimeout(ctx, 600*time.Second)
	defer reqCancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, "POST",
		strings.TrimRight(serviceURL, "/")+"/openai/process",
		bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, false
	}
	httpReq.Header.Set("Content-Type", "application/json")

	logf("[*] 等待远程注册...")

	// 心跳：每 10 秒轮询排队状态，向用户推送排队位置/ETA
	heartbeatDone := make(chan struct{})
	queueStatusURL := BuildQueueStatusURL(serviceURL, "openai")
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
		fmt.Printf("[DEBUG] 远程服务请求失败: %s\n", sanitizeHTTPError(err))
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
		fmt.Printf("[DEBUG] 远程服务返回 HTTP %d: %s\n", resp.StatusCode, snippet)
		return nil, false
	}

	var svcResp openaiServiceResponse
	if err := json.Unmarshal(body, &svcResp); err != nil {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		fmt.Printf("[DEBUG] 响应解析失败 (HTTP %d): %s | body: %s\n", resp.StatusCode, err, snippet)
		return nil, false
	}

	// 远程服务详细日志仅输出到 stdout 调试，不推送前端
	for _, remoteLog := range svcResp.Logs {
		fmt.Printf("[REMOTE] %s\n", remoteLog)
	}

	// 从远程日志中提取邮箱 provider 名，回写亲和度统计
	// 让下次 WeightedShuffleNames 能根据远程实际表现排序
	if provider := extractProviderFromRemoteLogs(svcResp.Logs); provider != "" {
		if svcResp.OK {
			tempmail.RecordSuccess(provider)
			fmt.Printf("[DEBUG] 亲和度: %s +success\n", provider)
		} else if isEmailRelatedFailure(svcResp.Error) {
			tempmail.RecordFailure(provider)
			fmt.Printf("[DEBUG] 亲和度: %s +fail (邮箱相关)\n", provider)
		}
		// 非邮箱原因的失败（403、网络等）不计入，避免污染邮箱统计
	}

	if !svcResp.OK {
		fmt.Printf("[DEBUG] 远程注册失败: %s\n", svcResp.Error)
		return nil, false
	}

	logf("[+] 远程注册成功: %s", svcResp.Email)

	// 解析完整 JSON 到 map，保留远程服务返回的所有 codex 字段
	result := map[string]interface{}{}
	json.Unmarshal(body, &result) //nolint:errcheck
	// 清理非 credential 字段
	delete(result, "ok")
	delete(result, "error")
	delete(result, "logs")

	// 防御校验：即使远程返回 ok=true，也必须包含 access_token，否则视为失败
	if result["access_token"] == nil || result["access_token"] == "" {
		fmt.Printf("[DEBUG] 远程返回 ok=true 但缺少 access_token，视为失败\n")
		return nil, false
	}

	return result, true
}

// sanitizeHTTPError 脱敏 HTTP 错误信息，移除内部 URL
func sanitizeHTTPError(err error) string {
	msg := err.Error()
	// 移除形如 Post "https://..." / Get "https://..." 中的 URL
	if i := strings.Index(msg, "\"http"); i >= 0 {
		if j := strings.Index(msg[i+1:], "\""); j >= 0 {
			msg = msg[:i] + "\"<remote>\"" + msg[i+1+j+1:]
		}
	}
	return msg
}

// ─── 子进程降级 ───

func (w *OpenAIWorker) callRegisterScript(ctx context.Context, email, password string, opts RegisterOpts, logf func(string, ...interface{})) (string, bool) {
	scriptPath := findScript("register_openai.py")
	if scriptPath == "" {
		return "注册服务未就绪（未配置 openai_reg_url 且找不到 register_openai.py）", false
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()

	args := []string{scriptPath, "--email", email, "--password", password}

	// 优先级：用户指定代理 > 系统代理
	if userProxy := opts.Config["user_proxy"]; userProxy != "" {
		args = append(args, "--proxy", userProxy)
	} else if opts.Proxy != nil {
		proxyStr := opts.Proxy.HTTPS
		if proxyStr == "" {
			proxyStr = opts.Proxy.HTTP
		}
		if proxyStr != "" {
			args = append(args, "--proxy", proxyStr)
		}
	}

	yydsMailURL := settingOrDefault(opts.Config, "yydsmail_base_url", "https://maliapi.215.im")
	yydsMailKey := opts.Config["yydsmail_api_key"]
	if yydsMailURL != "" && yydsMailKey != "" {
		args = append(args, "--yydsmail-url", yydsMailURL, "--yydsmail-key", yydsMailKey)
	}

	cfBypassURL := settingOrDefault(opts.Config, "cf_bypass_solver_url", "http://127.0.0.1:5073")
	if cfBypassURL != "" {
		args = append(args, "--cf-bypass-url", cfBypassURL)
	}

	cmd := exec.CommandContext(cmdCtx, "python3", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Sprintf("创建 stdout pipe 失败: %v", err), false
	}
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("启动脚本失败: %v", err), false
	}

	var resultLine string
	scanner := bufio.NewScanner(stdoutPipe)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "LOG:") {
			logf("[*] %s", strings.TrimPrefix(line, "LOG:"))
		} else if strings.HasPrefix(line, "OK:") || strings.HasPrefix(line, "FAIL:") {
			resultLine = line
		}
	}

	err = cmd.Wait()
	if err != nil || !strings.HasPrefix(resultLine, "OK:") {
		if errStr := strings.TrimSpace(stderr.String()); errStr != "" {
			lines := strings.Split(errStr, "\n")
			start := len(lines) - 5
			if start < 0 {
				start = 0
			}
			for _, l := range lines[start:] {
				if l = strings.TrimSpace(l); l != "" {
					logf("[dbg] %s", truncStr(l, 120))
				}
			}
		}
	}

	if strings.HasPrefix(resultLine, "OK:") {
		return strings.TrimPrefix(resultLine, "OK:"), true
	}
	if strings.HasPrefix(resultLine, "FAIL:") {
		return strings.TrimPrefix(resultLine, "FAIL:"), false
	}
	return truncStr(resultLine, 200), false
}

// findScript 查找 scripts/ 目录下的脚本文件
func findScript(name string) string {
	candidates := []string{filepath.Join("scripts", name)}
	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		candidates = append(candidates,
			filepath.Join(execDir, "scripts", name),
			filepath.Join(execDir, "..", "scripts", name),
		)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ctxSleep 在指定时间内休眠，若 ctx 被取消则立即返回
func ctxSleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// genOpenAIPassword 生成符合 OpenAI 密码要求的随机密码（使用 crypto/rand）
func genOpenAIPassword(length int) string {
	if length < 12 {
		length = 12
	}
	upper := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	lower := "abcdefghijklmnopqrstuvwxyz"
	digits := "0123456789"
	special := "!@#$%&*"
	all := upper + lower + digits + special

	cryptoRandByte := func(charset string) byte {
		n, _ := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(len(charset))))
		return charset[n.Int64()]
	}

	pw := make([]byte, length)
	pw[0] = cryptoRandByte(upper)
	pw[1] = cryptoRandByte(lower)
	pw[2] = cryptoRandByte(digits)
	pw[3] = cryptoRandByte(special)
	for i := 4; i < length; i++ {
		pw[i] = cryptoRandByte(all)
	}
	// Fisher-Yates shuffle with crypto/rand
	for i := len(pw) - 1; i > 0; i-- {
		j, _ := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(i+1)))
		pw[i], pw[j.Int64()] = pw[j.Int64()], pw[i]
	}
	return string(pw)
}

// ─── 辅助工具 ───

var _ = cryptorand.Read // 确保 import 不被剪枝

// extractProviderFromRemoteLogs 从远程服务日志中提取邮箱 provider 名
// 远程日志格式: "[*] 邮箱: xxx@yyy.com (via yydsmail)" 等
func extractProviderFromRemoteLogs(logs []string) string {
	for _, line := range logs {
		idx := strings.Index(line, "(via ")
		if idx < 0 {
			continue
		}
		rest := line[idx+5:]
		end := strings.Index(rest, ")")
		if end > 0 {
			return strings.TrimSpace(rest[:end])
		}
	}
	return ""
}

// isEmailRelatedFailure 判断远程注册失败是否与邮箱有关（验证码超时等）
// 非邮箱原因（403、网络超时、sentinel 等）不应计入邮箱 provider 的失败统计
func isEmailRelatedFailure(errMsg string) bool {
	emailKeywords := []string{"验证码超时", "验证码", "邮箱", "email", "mail", "verification", "code timeout"}
	lower := strings.ToLower(errMsg)
	for _, kw := range emailKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
