package tempmail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// YYDSMailProvider 基于 YYDS Mail API 的临时邮箱服务
// API: POST /v1/accounts → 创建邮箱（返回 address + token）
// API: GET  /v1/messages?address=xxx → 获取邮件列表（需 Bearer token）
// API: GET  /v1/messages/{id} → 获取邮件详情（需 Bearer token）
// API: DELETE /v1/accounts/{id} → 删除邮箱（需 Bearer token）
type YYDSMailProvider struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	mu         sync.Mutex
	lastCall   time.Time
	minDelay   time.Duration
}

const defaultYYDSMailBaseURL = "https://maliapi.215.im"

type yydsMessage struct {
	ID              string          `json:"id"`
	Subject         string          `json:"subject"`
	ReceivedAt      json.RawMessage `json:"received_at"`
	ReceivedAtCamel json.RawMessage `json:"receivedAt"`
	CreatedAt       json.RawMessage `json:"created_at"`
	CreatedAtCamel  json.RawMessage `json:"createdAt"`
	UpdatedAt       json.RawMessage `json:"updated_at"`
	Date            json.RawMessage `json:"date"`
	Timestamp       json.RawMessage `json:"timestamp"`
}

// NewYYDSMailProvider 创建 YYDS Mail provider
// baseURL: API 地址（如 https://<your-yydsmail-url>）
// apiKey: API Key（如 AC-xxxx）
func NewYYDSMailProvider(baseURL, apiKey string) *YYDSMailProvider {
	return &YYDSMailProvider{
		baseURL:    normalizeYYDSMailBaseURL(baseURL),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		minDelay:   300 * time.Millisecond,
	}
}

func (p *YYDSMailProvider) Name() string { return "yydsmail" }

// normalizeYYDSMailBaseURL 统一 YYDS Mail 基址。
// 文档和历史配置里同时出现过根域名和带 /v1 的写法，这里统一收敛到根域名，
// 后续请求始终自行拼接 /v1/*，避免出现 /v1/v1/accounts 这类双前缀问题。
func normalizeYYDSMailBaseURL(raw string) string {
	baseURL := strings.TrimRight(strings.TrimSpace(raw), "/")
	if baseURL == "" {
		return defaultYYDSMailBaseURL
	}
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	if baseURL == "" {
		return defaultYYDSMailBaseURL
	}
	return baseURL
}

// throttle 全局节流
func (p *YYDSMailProvider) throttle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Since(p.lastCall)
	if elapsed < p.minDelay {
		time.Sleep(p.minDelay - elapsed)
	}
	p.lastCall = time.Now()
}

// doRequest 执行 HTTP 请求，支持 API Key 或 Bearer token 认证
func (p *YYDSMailProvider) doRequest(ctx context.Context, method, rawURL string, authToken string, maxRetries int) ([]byte, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		p.throttle()

		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		} else {
			req.Header.Set("X-API-Key", p.apiKey)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")

		resp, err := p.httpClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(i+1) * 2 * time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, fmt.Errorf("%w: yydsmail HTTP %d", ErrAuthFailed, resp.StatusCode)
		}
		if resp.StatusCode == 429 {
			return nil, fmt.Errorf("%w: yydsmail 速率限制", ErrRateLimited)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("yydsmail HTTP %d: %s", resp.StatusCode, string(body))
			continue
		}
		return body, nil
	}
	return nil, fmt.Errorf("yydsmail 请求失败 (重试 %d 次): %w", maxRetries, lastErr)
}

// GenerateEmail 调用 POST /v1/accounts 创建临时邮箱
func (p *YYDSMailProvider) GenerateEmail(ctx context.Context) (string, map[string]string, error) {
	body, err := p.doRequest(ctx, "POST", p.baseURL+"/v1/accounts", "", 3)
	if err != nil {
		return "", nil, fmt.Errorf("yydsmail 创建邮箱失败: %w", err)
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			ID      string `json:"id"`
			Address string `json:"address"`
			Token   string `json:"token"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", nil, fmt.Errorf("解析 yydsmail 响应失败: %w", err)
	}
	if !resp.Success || resp.Data.Address == "" {
		return "", nil, fmt.Errorf("yydsmail 返回失败: %s", resp.Error)
	}

	meta := map[string]string{
		"provider": "yydsmail",
		"token":    resp.Data.Token,
		"inbox_id": resp.Data.ID,
	}
	return resp.Data.Address, meta, nil
}

// FetchVerificationCode 轮询 GET /v1/messages 获取验证码
func (p *YYDSMailProvider) FetchVerificationCode(ctx context.Context, addr string, meta map[string]string, maxAttempts int, interval time.Duration) (string, error) {
	token := meta["token"]
	if token == "" {
		return "", fmt.Errorf("yydsmail: 缺少 token")
	}

	listURL := fmt.Sprintf("%s/v1/messages?address=%s", p.baseURL, url.QueryEscape(addr))

	for i := 0; i < maxAttempts; i++ {
		body, err := p.doRequest(ctx, "GET", listURL, token, 1)
		if err != nil {
			time.Sleep(interval)
			continue
		}

		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Messages []yydsMessage `json:"messages"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil || !resp.Success {
			time.Sleep(interval)
			continue
		}

		messages := sortMessagesNewestFirst(resp.Data.Messages)

		// 按最新邮件优先遍历，先从 subject 提取，不行再获取详情。
		for _, msg := range messages {
			if code := ExtractVerificationCode(msg.Subject, ""); code != "" {
				return code, nil
			}

			// 获取邮件详情（含 text/html body）
			detailURL := fmt.Sprintf("%s/v1/messages/%s", p.baseURL, msg.ID)
			detailBody, err := p.doRequest(ctx, "GET", detailURL, token, 1)
			if err != nil {
				continue
			}

			var detail struct {
				Success bool                   `json:"success"`
				Data    map[string]interface{} `json:"data"`
			}
			if err := json.Unmarshal(detailBody, &detail); err != nil || !detail.Success {
				continue
			}

			for _, content := range collectDetailContents(detail.Data) {
				if code := ExtractVerificationCode("", content); code != "" {
					return code, nil
				}
			}
		}

		time.Sleep(interval)
	}
	return "", fmt.Errorf("yydsmail 获取验证码超时 (%d 次轮询)", maxAttempts)
}

func sortMessagesNewestFirst(messages []yydsMessage) []yydsMessage {
	sorted := append([]yydsMessage(nil), messages...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, okI := extractMessageTime(sorted[i])
		tj, okJ := extractMessageTime(sorted[j])
		if !(okI && okJ) {
			return false
		}
		if ti.Equal(tj) {
			return false
		}
		return ti.After(tj)
	})
	return sorted
}

func extractMessageTime(msg yydsMessage) (time.Time, bool) {
	for _, raw := range []json.RawMessage{
		msg.ReceivedAt,
		msg.ReceivedAtCamel,
		msg.CreatedAt,
		msg.CreatedAtCamel,
		msg.UpdatedAt,
		msg.Date,
		msg.Timestamp,
	} {
		if ts, ok := parseRawTimestamp(raw); ok {
			return ts, true
		}
	}
	return time.Time{}, false
}

func parseRawTimestamp(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return parseTimestampString(text)
	}

	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return parseTimestampNumber(number)
	}

	return time.Time{}, false
}

func parseTimestampString(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}

	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02T15:04:05.000Z07:00",
		"2006-01-02",
	} {
		if ts, err := time.Parse(layout, value); err == nil {
			return ts, true
		}
	}

	if number, err := strconv.ParseFloat(value, 64); err == nil {
		return parseTimestampNumber(number)
	}

	return time.Time{}, false
}

func parseTimestampNumber(value float64) (time.Time, bool) {
	if value <= 0 {
		return time.Time{}, false
	}

	seconds := int64(value)
	nanos := int64(0)
	if value > 1e12 {
		millis := int64(value)
		seconds = millis / 1000
		nanos = (millis % 1000) * int64(time.Millisecond)
	}
	return time.Unix(seconds, nanos), true
}

func collectDetailContents(data map[string]interface{}) []string {
	fields := []string{"text", "html", "content", "body", "text_content", "html_content"}
	contents := make([]string, 0, len(fields))
	for _, field := range fields {
		contents = appendContentValue(contents, data[field])
	}
	return contents
}

func appendContentValue(contents []string, value interface{}) []string {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			return append(contents, v)
		}
	case []interface{}:
		for _, item := range v {
			contents = appendContentValue(contents, item)
		}
	case []string:
		for _, item := range v {
			contents = appendContentValue(contents, item)
		}
	}
	return contents
}

// DeleteEmail 调用 DELETE /v1/accounts/{id} 删除邮箱（需要 temp token 认证）
func (p *YYDSMailProvider) DeleteEmail(ctx context.Context, addr string, meta map[string]string) error {
	inboxID := meta["inbox_id"]
	token := meta["token"]
	if inboxID == "" || token == "" {
		return nil
	}
	deleteURL := fmt.Sprintf("%s/v1/accounts/%s", p.baseURL, inboxID)
	_, _ = p.doRequest(ctx, "DELETE", deleteURL, token, 1)
	return nil
}
