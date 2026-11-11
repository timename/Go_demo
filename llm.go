package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ══════════════════════════════════════════════════════════
// 请求/响应结构体（对应 OpenAI Responses 协议）
// ══════════════════════════════════════════════════════════

type chatRequest struct {
	Question string `json:"question"`
}

type chatResponse struct {
	Answer      string `json:"answer"`
	Model       string `json:"model"`
	Attempts    int    `json:"attempts"`     // 实际尝试了几次
	LatencyMS   int64  `json:"latency_ms"`   // 端到端耗时
	PromptChars int    `json:"prompt_chars"` // 输入字符数（粗略体现成本）
}

// LLMClient 封装对大模型接口的访问。
//
// 负责：鉴权、超时、重试、错误分类、流式解析。
// 不负责：HTTP 路由、页面渲染——那些交给 handlers.go。
type LLMClient struct {
	cfg        *Config
	httpClient *http.Client
}

func NewLLMClient(cfg *Config) *LLMClient {
	return &LLMClient{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
			Transport: &http.Transport{
				// 强制不走系统代理：本机挂了代理软件时，
				// Go 的默认 Transport 会读取 HTTP_PROXY，
				// 而该代理转发本接口时会中断连接（表现为 EOF）。
				Proxy: nil,
			},
		},
	}
}

// apiError 表达一次可诊断的失败。
// Retryable 决定是否值得重试——重试所有错误只会浪费时间和额度。
type apiError struct {
	Status    int
	Retryable bool
	Message   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("LLM 调用失败(status=%d, retryable=%t): %s", e.Status, e.Retryable, e.Message)
}

// buildPayload 组装请求体。
func (c *LLMClient) buildPayload(question string, stream bool) map[string]any {
	payload := map[string]any{
		"model": c.cfg.Model,
		"input": []map[string]any{
			{
				"role": "system",
				"content": []map[string]any{
					{"type": "input_text", "text": c.cfg.SystemRule},
				},
			},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": question},
				},
			},
		},
		"max_output_tokens": c.cfg.MaxTokens,
		"temperature":       0,
	}
	if stream {
		payload["stream"] = true
	}
	return payload
}

// Ask 同步提问，带指数退避重试。
func (c *LLMClient) Ask(ctx context.Context, question string) (*chatResponse, error) {
	start := time.Now()
	payload := c.buildPayload(question, false)

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxRetries; attempt++ {
		answer, err := c.doOnce(ctx, payload)
		if err == nil {
			return &chatResponse{
				Answer:      answer,
				Model:       c.cfg.Model,
				Attempts:    attempt,
				LatencyMS:   time.Since(start).Milliseconds(),
				PromptChars: len(question),
			}, nil
		}
		lastErr = err

		var ae *apiError
		if errors.As(err, &ae) && !ae.Retryable {
			// 不可重试（如 401 鉴权失败、400 参数错误），立刻返回
			break
		}
		if attempt == c.cfg.MaxRetries {
			break
		}
		// 指数退避：1s、2s、4s……上限 8s
		backoff := time.Duration(1<<uint(attempt-1)) * time.Second
		if backoff > 8*time.Second {
			backoff = 8 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

// doOnce 发一次请求，解析非流式响应。
func (c *LLMClient) doOnce(ctx context.Context, payload map[string]any) (string, error) {
	resp, err := c.post(ctx, payload)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", &apiError{Status: resp.StatusCode, Retryable: true, Message: "读取响应失败: " + err.Error()}
	}

	if resp.StatusCode != http.StatusOK {
		return "", &apiError{
			Status:    resp.StatusCode,
			Retryable: isRetryableStatus(resp.StatusCode),
			Message:   truncate(string(body), 300),
		}
	}

	return parseResponse(body)
}

// post 统一构造并发送请求。
func (c *LLMClient) post(ctx context.Context, payload map[string]any) (*http.Response, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// 网络层错误（超时、连接中断）通常值得重试
		return nil, &apiError{Status: 0, Retryable: true, Message: err.Error()}
	}
	return resp, nil
}

// isRetryableStatus：5xx 与 429 值得重试；4xx 参数/鉴权问题重试无意义。
func isRetryableStatus(code int) bool {
	if code == http.StatusTooManyRequests {
		return true
	}
	return code >= 500
}

// parseResponse 从 OpenAI Responses 协议的结构里取出文本。
func parseResponse(body []byte) (string, error) {
	var decoded struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("解析响应失败: %w，原始内容: %s", err, truncate(string(body), 200))
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		return "", fmt.Errorf("接口返回错误: %s", decoded.Error.Message)
	}

	var sb strings.Builder
	for _, item := range decoded.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				sb.WriteString(c.Text)
			}
		}
	}

	answer := strings.TrimSpace(sb.String())
	if answer == "" {
		return "", fmt.Errorf("模型未返回文本内容")
	}
	return answer, nil
}

// ══════════════════════════════════════════════════════════
// 流式（SSE）调用
// ══════════════════════════════════════════════════════════

// StreamChunk 是推给前端的一个片段。
type StreamChunk struct {
	Type  string `json:"type"`            // delta / done / error
	Text  string `json:"text,omitempty"`  // 增量文本
	Model string `json:"model,omitempty"` // 模型名（done 时带）
}

// AskStream 发起流式请求，每读到一段增量就调用 onChunk。
//
// 注意：流式一旦开始推送，中途出错就不能再重试（客户端已收到部分内容），
// 所以这里不做重试，只把错误作为 error 片段传出。
func (c *LLMClient) AskStream(ctx context.Context, question string, onChunk func(StreamChunk)) error {
	payload := c.buildPayload(question, true)

	resp, err := c.post(ctx, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &apiError{
			Status:    resp.StatusCode,
			Retryable: isRetryableStatus(resp.StatusCode),
			Message:   truncate(string(body), 300),
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		// 上游的流式事件格式较多样，这里宽松解析：
		// 只要 JSON 里能找到文本增量就推送，找不到就跳过。
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		if text := extractDelta(event); text != "" {
			onChunk(StreamChunk{Type: "delta", Text: text})
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取流失败: %w", err)
	}

	onChunk(StreamChunk{Type: "done", Model: c.cfg.Model})
	return nil
}

// extractDelta 从流式事件里尽力提取文本增量。
// 兼容两类常见写法：
// extractDelta 从流式事件里尽力提取文本增量。
//
// 不同上游/不同协议的字段名不一致，这里按"能取到就用"的顺序兜底：
//  1. {"delta": "..."}                                    OpenAI Responses
//  2. {"delta": {"text": "..."}}                          部分兼容实现
//  3. {"delta": {"content": "..."}}                       同上
//  4. {"type": "response.output_text.delta", "text": "..."}  字段名在顶层
//  5. {"choices": [{"delta": {"content": "..."}}]}         OpenAI Chat Completions
//
// 返回空串表示这不是一个文本增量事件（如 response.completed），调用方跳过即可。
func extractDelta(event map[string]any) string {
	// 1~3：delta 字段，可能是字符串也可能是对象
	if raw, ok := event["delta"]; ok {
		switch v := raw.(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			for _, key := range []string{"text", "content"} {
				if s, ok := v[key].(string); ok && s != "" {
					return s
				}
			}
		}
	}

	// 4：文本直接放在事件顶层（配合 type 判断，避免误取其他字段）
	if t, ok := event["type"].(string); ok && strings.HasSuffix(t, "output_text.delta") {
		if s, ok := event["text"].(string); ok && s != "" {
			return s
		}
	}

	// 5：Chat Completions 风格
	if choices, ok := event["choices"].([]any); ok {
		for _, raw := range choices {
			choice, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			if content, ok := delta["content"].(string); ok && content != "" {
				return content
			}
		}
	}

	return ""
}

// truncate 截断过长文本，避免把整段响应塞进日志。
// 按字符截断而不是按字节，否则可能把一个中文字切成两半产生乱码。
func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "...(truncated)"
}
