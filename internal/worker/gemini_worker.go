package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/xiaolajiaoyyds/regplatformm/internal/pkg/tempmail"
)

// GeminiWorker Gemini Business 平台注册器
// 注册流程：临时邮箱 → 调用 Python 浏览器服务 → Camoufox 完成 Google 登录 → 提取 Cookies/Session
// 复用 aws-builder-id-reg 服务（/gemini/process 端点），共享 Camoufox 浏览器基础设施
type GeminiWorker struct{}

func init() {
	Register(&GeminiWorker{})
}

func (w *GeminiWorker) PlatformName() string { return "gemini" }

// ScanConfig Gemini 无需预扫描
func (w *GeminiWorker) ScanConfig(_ context.Context, _ *ProxyEntry, cfg Config) (Config, error) {
	return cfg, nil
}

// RegisterOne 执行一次 Gemini Business 注册
func (w *GeminiWorker) RegisterOne(ctx context.Context, opts RegisterOpts) {
	cfg := opts.Config

	// ── 读取注册服务地址（复用 Kiro 的 Python 服务，增加 /gemini/process 端点）──
	geminiRegURL := strings.TrimRight(cfg["gemini_reg_url"], "/")
	if geminiRegURL == "" {
		// 默认与 Kiro 服务同地址（同一个 Python 进程提供 /kiro/process 和 /gemini/process）
		geminiRegURL = strings.TrimRight(cfg["kiro_reg_url"], "/")
	}
	if geminiRegURL == "" {
		logSend(opts.LogCh, "[!] Gemini 注册服务地址未配置（gemini_reg_url 或 kiro_reg_url），请在系统设置中填写")
		opts.OnFail()
		return
	}

	// ── Step 1: 通过 MultiProvider 创建临时邮箱 ──
	logSend(opts.LogCh, "[*] Gemini 注册开始，创建临时邮箱...")

	// 复制 config map，避免并发修改共享数据（data race）
	localCfg := make(Config, len(cfg))
	for k, v := range cfg {
		localCfg[k] = v
	}
	if platformProviders := localCfg["gemini_email_providers"]; platformProviders != "" {
		localCfg["email_provider_priority"] = platformProviders
	}
	mailProvider := tempmail.NewMultiProvider(localCfg)
	email, mailMeta, err := mailProvider.GenerateEmail(ctx)
	if err != nil {
		logSend(opts.LogCh, fmt.Sprintf("[!] 创建邮箱失败: %v", err))
		opts.OnFail()
		return
	}
	logSend(opts.LogCh, fmt.Sprintf("[+] 邮箱已创建: %s (via %s)", email, mailMeta["provider"]))

	// ── 随机启动抖动，分散并发请求 ──
	jitter := time.Duration(rand.Intn(3000)) * time.Millisecond
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	// ── Step 2: 组装代理参数 ──
	proxyStr := ""
	if userProxy := cfg["user_proxy"]; userProxy != "" {
		proxyStr = userProxy
		logSend(opts.LogCh, "[*] 使用用户指定代理")
	} else if geminiProxy := cfg["gemini_proxy"]; geminiProxy != "" {
		proxyStr = geminiProxy
	} else if opts.Proxy != nil && opts.Proxy.HTTP != "" {
		proxyStr = opts.Proxy.HTTP
	}

	// ── Step 3: 调用浏览器注册服务（最多重试 3 次，每次换新邮箱）──
	const maxRetry = 3
	var result *geminiRegResponse
	var rawJSON json.RawMessage
	var callErr error
	for attempt := 1; attempt <= maxRetry; attempt++ {
		if attempt > 1 {
			logSend(opts.LogCh, fmt.Sprintf("[*] 第 %d 次重试（换新邮箱）...", attempt))
			newEmail, newMeta, newErr := mailProvider.GenerateEmail(ctx)
			if newErr != nil {
				logSend(opts.LogCh, fmt.Sprintf("[!] 重试创建邮箱失败: %v", newErr))
				break
			}
			go func(e string, m map[string]string) {
				dCtx, dCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer dCancel()
				mailProvider.DeleteEmail(dCtx, e, m)
			}(email, mailMeta)
			email = newEmail
			mailMeta = newMeta
			logSend(opts.LogCh, fmt.Sprintf("[+] 重试邮箱: %s (via %s)", email, mailMeta["provider"]))
		}
		logSend(opts.LogCh, "[*] 调用浏览器注册服务（Camoufox 指纹伪造）...")
		result, rawJSON, callErr = w.callRegService(ctx, geminiRegURL, email, proxyStr, mailMeta, cfg, opts.LogCh)
		if callErr != nil {
			logSend(opts.LogCh, fmt.Sprintf("[!] 注册服务调用失败: %v", callErr))
			break
		}
		if result.OK || !result.Retriable {
			break
		}
		logSend(opts.LogCh, fmt.Sprintf("[!] 注册被拒绝（可重试）: %s，准备重试...", result.Error))
	}

	// ── Step 4: 处理结果 ──
	if callErr != nil {
		go func(e string, m map[string]string) {
			dCtx, dCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer dCancel()
			mailProvider.DeleteEmail(dCtx, e, m)
		}(email, mailMeta)
		opts.OnFail()
		return
	}
	if result == nil || !result.OK {
		errMsg := ""
		if result != nil {
			errMsg = result.Error
		}
		logSend(opts.LogCh, fmt.Sprintf("[!] Gemini 注册失败: %s", errMsg))
		go func(e string, m map[string]string) {
			dCtx, dCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer dCancel()
			mailProvider.DeleteEmail(dCtx, e, m)
		}(email, mailMeta)
		opts.OnFail()
		return
	}

	// 解析完整 JSON，保留远程服务返回的所有字段
	credential := map[string]interface{}{}
	if err := json.Unmarshal(rawJSON, &credential); err != nil {
		logSend(opts.LogCh, fmt.Sprintf("[!] 响应 JSON 解析异常: %v", err))
		go func(e string, m map[string]string) {
			dCtx, dCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer dCancel()
			mailProvider.DeleteEmail(dCtx, e, m)
		}(email, mailMeta)
		opts.OnFail()
		return
	}
	// 校验关键凭证字段（防止空壳数据入库）
	if credential["email"] == nil || credential["config_id"] == nil {
		logSend(opts.LogCh, "[!] 远程服务返回不完整凭证（缺少 email/config_id），跳过保存")
		go func(e string, m map[string]string) {
			dCtx, dCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer dCancel()
			mailProvider.DeleteEmail(dCtx, e, m)
		}(email, mailMeta)
		opts.OnFail()
		return
	}
	delete(credential, "ok")
	delete(credential, "error")
	delete(credential, "retriable")
	delete(credential, "logs")
	// 补充元数据
	credential["platform"] = "gemini"
	credential["type"] = "gemini-business"
	credential["auth_method"] = "google-otp"
	credential["provider"] = "Google"
	credential["disabled"] = false
	credential["mail_provider"] = mailMeta["provider"]
	tempmail.RecordSuccess(mailMeta["provider"])
	emailMetaCopy := make(map[string]interface{}, len(mailMeta))
	for k, v := range mailMeta {
		emailMetaCopy[k] = v
	}
	credential["email_meta"] = emailMetaCopy

	if result.Warning != "" {
		credential["warning"] = result.Warning
		logSend(opts.LogCh, fmt.Sprintf("[~] 提示: %s", result.Warning))
	}

	logSend(opts.LogCh, fmt.Sprintf("[+] Gemini 注册成功: %s (config_id=%s)", result.Email, result.ConfigID))

	delCtx, delCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer delCancel()
	if err := mailProvider.DeleteEmail(delCtx, email, mailMeta); err != nil {
		log.Warn().Str("email", email).Err(err).Msg("Gemini: 清理邮箱失败（非关键）")
	}

	opts.OnSuccess(result.Email, credential)
}

// ─── 内部类型 ────────────────────────────────────────────────────────────────

// geminiRegRequest Python 注册服务请求体
type geminiRegRequest struct {
	Email        string            `json:"email"`
	Proxy        string            `json:"proxy,omitempty"`
	YYDSMailURL  string            `json:"yydsmail_url,omitempty"`
	YYDSMailKey  string            `json:"yydsmail_key,omitempty"`
	MailProvider string            `json:"mail_provider,omitempty"`
	MailMeta     map[string]string `json:"mail_meta,omitempty"`
}

// geminiRegResponse Python 注册服务响应体
type geminiRegResponse struct {
	OK           bool   `json:"ok"`
	Email        string `json:"email"`
	ConfigID     string `json:"config_id"`
	Csesidx      string `json:"csesidx"`
	CSes         string `json:"c_ses"`
	COses        string `json:"c_oses"`
	TrialEndDate string `json:"trial_end_date"`
	ExpiresAt    string `json:"expires_at"`
	Warning      string `json:"warning"`
	Error        string `json:"error"`
	Retriable    bool   `json:"retriable"`
}

// ─── 内部方法 ────────────────────────────────────────────────────────────────

// callRegService 调用 Python 服务执行 Gemini 浏览器注册（流式日志协议）
func (w *GeminiWorker) callRegService(
	ctx context.Context,
	baseURL, email, proxy string,
	mailMeta map[string]string,
	cfg Config,
	logCh chan<- string,
) (*geminiRegResponse, json.RawMessage, error) {
	reqBody := geminiRegRequest{
		Email:        email,
		Proxy:        proxy,
		YYDSMailURL:  normalizeRegYYDSMailBaseURL(cfg["yydsmail_base_url"]),
		YYDSMailKey:  cfg["yydsmail_api_key"],
		MailProvider: mailMeta["provider"],
		MailMeta:     mailMeta,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// Python 端 RESPONSE_TIMEOUT=400s + 浏览器启动/关闭开销，Go 侧需要留足余量
	callCtx, cancel := context.WithTimeout(ctx, 7*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		baseURL+"/gemini/process", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("构建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 安全兜底超时（比 context 的 7 分钟略长），防止 context cancel 泄漏时连接永久挂起
	client := &http.Client{Timeout: 8 * time.Minute}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var preview [200]byte
		n, _ := resp.Body.Read(preview[:])
		return nil, nil, fmt.Errorf("服务返回 HTTP %d: %s", resp.StatusCode, string(preview[:n]))
	}

	// 逐行读取流式响应：LOG: 前缀为日志，最后一行为 JSON 结果
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var lastLine string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "LOG:") {
			msg := strings.TrimSpace(strings.TrimPrefix(line, "LOG:"))
			if msg != "" && msg != "." {
				logSend(logCh, "[~] "+msg)
			}
		} else if line != "" {
			lastLine = line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("读取流式响应失败: %w", err)
	}
	if lastLine == "" {
		return nil, nil, fmt.Errorf("服务未返回结果行")
	}

	var result geminiRegResponse
	if err := json.Unmarshal([]byte(lastLine), &result); err != nil {
		return nil, nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &result, json.RawMessage(lastLine), nil
}
