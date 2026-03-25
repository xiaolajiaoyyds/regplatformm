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

// KiroWorker AWS Kiro（Builder ID）平台注册器
// 注册流程：GPTMail 生成邮箱 → 调用 aws-builder-id-reg Python 服务 → 浏览器完成注册
// Python 服务内置全维度指纹伪造（Canvas/WebGL/Audio/Navigator/Screen/WebRTC）及人类行为模拟
type KiroWorker struct{}

func init() {
	Register(&KiroWorker{})
}

func (w *KiroWorker) PlatformName() string { return "kiro" }

// ScanConfig Kiro 无需预扫描（注册页结构固定，无需提取 site_key 等动态配置）
func (w *KiroWorker) ScanConfig(_ context.Context, _ *ProxyEntry, cfg Config) (Config, error) {
	return cfg, nil
}

// RegisterOne 执行一次 Kiro 注册
func (w *KiroWorker) RegisterOne(ctx context.Context, opts RegisterOpts) {
	cfg := opts.Config

	// ── 读取必要配置 ──────────────────────────────────────────────────────
	kiroRegURL := strings.TrimRight(cfg["kiro_reg_url"], "/")

	if kiroRegURL == "" {
		logSend(opts.LogCh, "[!] Kiro 注册服务地址未配置（kiro_reg_url），请在系统设置中填写")
		opts.OnFail()
		return
	}

	// ── Step 1: 通过 MultiProvider 创建临时邮箱（支持自动降级）──────────
	logSend(opts.LogCh, "[*] Kiro 注册开始，创建临时邮箱...")

	// 平台专属邮箱 provider 覆盖全局优先级
	if platformProviders := cfg["kiro_email_providers"]; platformProviders != "" {
		cfg["email_provider_priority"] = platformProviders
	}
	mailProvider := tempmail.NewMultiProvider(opts.Config)
	email, mailMeta, err := mailProvider.GenerateEmail(ctx)
	if err != nil {
		logSend(opts.LogCh, fmt.Sprintf("[!] 创建邮箱失败: %v", err))
		opts.OnFail()
		return
	}
	logSend(opts.LogCh, fmt.Sprintf("[+] 邮箱已创建: %s (via %s)", email, mailMeta["provider"]))

	// ── 随机启动抖动，分散并发请求 ────────────────────────────────────────
	jitter := time.Duration(rand.Intn(3000)) * time.Millisecond
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	// ── Step 2: 组装代理参数 ──────────────────────────────────────────────
	proxyStr := ""
	// 优先级：用户指定代理 > Kiro 专用代理 > 任务级代理
	if userProxy := cfg["user_proxy"]; userProxy != "" {
		proxyStr = userProxy
		logSend(opts.LogCh, "[*] 使用用户指定代理")
	} else if kiroProxy := cfg["kiro_proxy"]; kiroProxy != "" {
		proxyStr = kiroProxy
	} else if opts.Proxy != nil && opts.Proxy.HTTP != "" {
		proxyStr = opts.Proxy.HTTP
	}

	// ── Step 3: 调用 aws-builder-id-reg Python 服务（流式日志）────────────
	// retriable 错误最多重试 3 次（换新邮箱，同一代理）
	const maxRetry = 3
	var result *kiroRegResponse
	var rawJSON json.RawMessage
	var callErr error
	for attempt := 1; attempt <= maxRetry; attempt++ {
		if attempt > 1 {
			logSend(opts.LogCh, fmt.Sprintf("[*] 第 %d 次重试（换新邮箱）...", attempt))
			// 换新邮箱（通过 MultiProvider，自动降级）
			newEmail, newMeta, newErr := mailProvider.GenerateEmail(ctx)
			if newErr != nil {
				logSend(opts.LogCh, fmt.Sprintf("[!] 重试创建邮箱失败: %v", newErr))
				break
			}
			go mailProvider.DeleteEmail(context.Background(), email, mailMeta)
			email = newEmail
			mailMeta = newMeta
			logSend(opts.LogCh, fmt.Sprintf("[+] 重试邮箱: %s (via %s)", email, mailMeta["provider"]))
		}
		logSend(opts.LogCh, "[*] 调用浏览器注册服务（指纹伪造 + 行为模拟）...")
		result, rawJSON, callErr = w.callRegService(ctx, kiroRegURL, email, proxyStr, mailMeta, cfg, opts.LogCh)
		if callErr != nil {
			logSend(opts.LogCh, fmt.Sprintf("[!] 注册服务调用失败: %v", callErr))
			break
		}
		if result.OK || !result.Retriable {
			break // 成功或不可重试的错误，直接退出
		}
		logSend(opts.LogCh, fmt.Sprintf("[!] 注册被拒绝（可重试）: %s，准备重试...", result.Error))
	}
	// ── Step 4: 处理结果 ──────────────────────────────────────────────────
	if callErr != nil {
		go mailProvider.DeleteEmail(context.Background(), email, mailMeta)
		opts.OnFail()
		return
	}
	if result == nil || !result.OK {
		errMsg := ""
		if result != nil {
			errMsg = result.Error
		}
		logSend(opts.LogCh, fmt.Sprintf("[!] Kiro 注册失败: %s", errMsg))
		go mailProvider.DeleteEmail(context.Background(), email, mailMeta)
		opts.OnFail()
		return
	}

	// 解析完整 JSON，保留远程服务返回的所有字段
	credential := map[string]interface{}{}
	json.Unmarshal(rawJSON, &credential) //nolint:errcheck
	// 清理控制字段
	delete(credential, "ok")
	delete(credential, "error")
	delete(credential, "retriable")
	delete(credential, "logs")
	// 补充元数据
	credential["platform"] = "kiro"
	credential["type"] = "kiro"
	credential["auth_method"] = "builder-id"
	credential["provider"] = "AWS"
	credential["disabled"] = false
	// 保存邮箱 provider 信息，供后续 FetchOTP 使用
	credential["mail_provider"] = mailMeta["provider"]
	tempmail.RecordSuccess(mailMeta["provider"])
	emailMetaCopy := make(map[string]interface{}, len(mailMeta))
	for k, v := range mailMeta {
		emailMetaCopy[k] = v
	}
	credential["email_meta"] = emailMetaCopy
	if _, ok := credential["profile_arn"]; !ok {
		credential["profile_arn"] = ""
	}
	if _, ok := credential["start_url"]; !ok {
		credential["start_url"] = ""
	}
	if result.Warning != "" {
		credential["warning"] = result.Warning
		logSend(opts.LogCh, fmt.Sprintf("[~] 提示: %s", result.Warning))
	}

	// 日志只记录邮箱，不打印密码（密码通过 OnSuccess 安全存储）
	logSend(opts.LogCh, fmt.Sprintf("[+] Kiro 注册成功: %s", result.Email))

	// 清理邮箱（非关键步骤，失败不影响结果）
	if err := mailProvider.DeleteEmail(context.Background(), email, mailMeta); err != nil {
		log.Warn().Str("email", email).Err(err).Msg("Kiro: 清理邮箱失败（非关键）")
	}

	opts.OnSuccess(result.Email, credential)
}

// ─── 内部类型 ────────────────────────────────────────────────────────────────

// kiroRegRequest aws-builder-id-reg 服务请求体
type kiroRegRequest struct {
	Email        string            `json:"email"`
	Proxy        string            `json:"proxy,omitempty"`
	YYDSMailURL  string            `json:"yydsmail_url,omitempty"`
	YYDSMailKey  string            `json:"yydsmail_key,omitempty"`
	Region       string            `json:"region"`
	MailProvider string            `json:"mail_provider,omitempty"` // 邮箱 provider 类型（如 "yydsmail"）
	MailMeta     map[string]string `json:"mail_meta,omitempty"`     // provider 特有元数据（token/sid_token 等）
}

// kiroRegResponse aws-builder-id-reg 服务响应体
type kiroRegResponse struct {
	OK           bool   `json:"ok"`
	Email        string `json:"email"`
	Password     string `json:"password"`
	Name         string `json:"name"`
	Warning      string `json:"warning"`
	Error        string `json:"error"`
	Retriable    bool   `json:"retriable"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	ExpiresAt    string `json:"expires_at"`
	LastRefresh  string `json:"last_refresh"`
	Region       string `json:"region"`
}

// ─── 内部方法 ────────────────────────────────────────────────────────────────

// logSend 非阻塞写入日志通道（防止消费者退出时 goroutine 泄漏）
// 使用 recover 防止向已关闭 channel 写入导致 panic（inflight worker 可能在 LogCh 关闭后仍在运行）
func logSend(ch chan<- string, msg string) {
	defer func() { recover() }()
	select {
	case ch <- msg:
	default:
	}
}

// callRegService 调用 aws-builder-id-reg Python 服务执行浏览器注册。
// 响应为流式文本：每行 "LOG:消息" 是实时日志，最后一行是 JSON 结果。
func (w *KiroWorker) callRegService(
	ctx context.Context,
	baseURL, email, proxy string,
	mailMeta map[string]string,
	cfg Config,
	logCh chan<- string,
) (*kiroRegResponse, json.RawMessage, error) {
	reqBody := kiroRegRequest{
		Email:        email,
		Proxy:        proxy,
		YYDSMailURL:  normalizeRegYYDSMailBaseURL(cfg["yydsmail_base_url"]),
		YYDSMailKey:  cfg["yydsmail_api_key"],
		Region:       "usa",
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
		baseURL+"/kiro/process", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("构建 HTTP 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		// 不设 Timeout：流式响应会持续很久，由 context 控制超时
		Timeout: 0,
	}

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

	// 逐行读取流式响应
	// - "LOG:xxx" 行：实时转发到前端日志
	// - 最后一行（非 LOG: 前缀）：JSON 结果
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

	var result kiroRegResponse
	if err := json.Unmarshal([]byte(lastLine), &result); err != nil {
		return nil, nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &result, json.RawMessage(lastLine), nil
}
