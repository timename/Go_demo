package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ══════════════════════════════════════════════════════════
// 统计：满足 JD 里「日志、监控、成本与延迟统计」的要求
// ══════════════════════════════════════════════════════════

// Stats 记录服务运行期间的调用情况。
//
// 用 sync/atomic 而非 Mutex：这里的字段互相独立，
// 统计数字短暂不一致没有业务影响，原子操作比锁更轻。
type Stats struct {
	total        atomic.Int64 // 总请求数
	succeeded    atomic.Int64 // 成功数
	failed       atomic.Int64 // 失败数
	totalLatency atomic.Int64 // 累计耗时（毫秒）
	promptChars  atomic.Int64 // 累计输入字符数
	startedAt    time.Time
}

func NewStats() *Stats {
	return &Stats{startedAt: time.Now()}
}

// Record 记录一次调用的结果。
func (s *Stats) Record(ok bool, latency time.Duration, promptChars int) {
	s.total.Add(1)
	if ok {
		s.succeeded.Add(1)
	} else {
		s.failed.Add(1)
	}
	s.totalLatency.Add(latency.Milliseconds())
	s.promptChars.Add(int64(promptChars))
}

// Snapshot 返回一份快照，供 JSON 输出。
func (s *Stats) Snapshot() map[string]any {
	total := s.total.Load()
	avg := int64(0)
	if total > 0 {
		avg = s.totalLatency.Load() / total
	}
	return map[string]any{
		"total":          total,
		"succeeded":      s.succeeded.Load(),
		"failed":         s.failed.Load(),
		"avg_latency_ms": avg,
		"prompt_chars":   s.promptChars.Load(),
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
	}
}

// ══════════════════════════════════════════════════════════
// Server 把配置、客户端、统计绑在一起
// ══════════════════════════════════════════════════════════

type Server struct {
	cfg    *Config
	llm    *LLMClient
	stats  *Stats
	static string // 静态文件目录
}

func NewServer(cfg *Config) *Server {
	return &Server{
		cfg:    cfg,
		llm:    NewLLMClient(cfg),
		stats:  NewStats(),
		static: "static",
	}
}

// Register 注册所有路由。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/chat", s.handleChat)              // POST 同步问答
	mux.HandleFunc("/chat/stream", s.handleChatStream) // GET 流式问答
	mux.HandleFunc("/demo", s.handleDemo)              // 前端演示页
}

// ── 根路径 ────────────────────────────────────────────────
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// 浏览器会自动请求 favicon，直接回 204，避免控制台报 404
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "Go LLM Gateway")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "接口：")
	fmt.Fprintln(w, "  GET  /health        健康检查")
	fmt.Fprintln(w, "  GET  /stats         调用统计")
	fmt.Fprintln(w, "  POST /chat          同步问答  {\"question\":\"...\"}")
	fmt.Fprintln(w, "  GET  /chat/stream   流式问答  ?q=...")
	fmt.Fprintln(w, "  GET  /demo          演示页面")
}

// ── 健康检查 ──────────────────────────────────────────────
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"model":  s.cfg.Model,
		"time":   time.Now().Format(time.RFC3339),
	})
}

// ── 统计 ──────────────────────────────────────────────────
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.stats.Snapshot())
}

// ── 同步问答 ──────────────────────────────────────────────
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "请使用 POST")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}

	question := normalizeQuestion(req.Question)
	if err := validateQuestion(question); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	start := time.Now()
	resp, err := s.llm.Ask(r.Context(), question)
	latency := time.Since(start)

	if err != nil {
		s.stats.Record(false, latency, charLen(question))
		log.Printf("[%s] [chat] 失败 question=%q err=%v", reqID, truncate(question, 50), err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.stats.Record(true, latency, charLen(question))
	log.Printf("[%s] [chat] 成功 attempts=%d latency=%dms chars=%d",
		reqID, resp.Attempts, resp.LatencyMS, charLen(question))
	writeJSON(w, http.StatusOK, resp)
}

// ── 流式问答（SSE）────────────────────────────────────────
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFrom(r.Context())

	question := normalizeQuestion(r.URL.Query().Get("q"))
	if err := validateQuestion(question); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前服务器不支持流式响应")
		return
	}

	// SSE 必须的响应头
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关闭 nginx 缓冲

	start := time.Now()
	chunks := 0
	sentAny := false // 是否已推出过内容
	clientGone := false

	// send 返回错误，让调用方能感知"客户端已断开"
	send := func(chunk StreamChunk) error {
		raw, err := json.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("序列化事件失败: %w", err)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return fmt.Errorf("写入响应失败（客户端可能已断开）: %w", err)
		}
		flusher.Flush()
		return nil
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	streamErr := s.llm.AskStream(ctx, question, func(c StreamChunk) {
		if clientGone {
			return // 客户端已走，不再尝试写入
		}
		if c.Type == "delta" {
			chunks++
			sentAny = true
		}
		if err := send(c); err != nil {
			clientGone = true
			log.Printf("[%s] [stream] 推送失败，停止: %v", reqID, err)
		}
	})

	switch {
	case clientGone:
		s.stats.Record(false, time.Since(start), charLen(question))
		log.Printf("[%s] [stream] 客户端断开 chunks=%d", reqID, chunks)
		return

	case streamErr != nil:
		s.stats.Record(false, time.Since(start), charLen(question))
		log.Printf("[%s] [stream] 失败 chunks=%d err=%v", reqID, chunks, streamErr)

		if !sentAny {
			// 还没推出任何内容，可以改用 HTTP 错误码返回，前端能直接判断失败
			w.Header().Del("Content-Type")
			writeError(w, http.StatusBadGateway, streamErr.Error())
			return
		}
		// 已有部分输出，只能再推一个 error 事件
		_ = send(StreamChunk{Type: "error", Text: "输出中断: " + streamErr.Error()})
		return

	default:
		s.stats.Record(true, time.Since(start), charLen(question))
		log.Printf("[%s] [stream] 成功 chunks=%d latency=%dms",
			reqID, chunks, time.Since(start).Milliseconds())
	}
}

// ── 演示页 ────────────────────────────────────────────────
func (s *Server) handleDemo(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.static, "index.html")
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "演示页未找到，请确认 static/index.html 存在")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeFile(w, r, path)
}

// ══════════════════════════════════════════════════════════
// 工具函数
// ══════════════════════════════════════════════════════════

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("写响应失败: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// normalizeQuestion 清理用户输入。
//
// 用 strings.Fields 而不是手写循环：它会按 Unicode 空白切分，
// 能正确处理全角空格（\u3000）、制表符、换行等，再统一用单个空格连接。
func normalizeQuestion(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// 问题长度上限（按字符数，不是字节数）
const maxQuestionChars = 2000

// validateQuestion 校验输入。
//
// 用 utf8.RuneCountInString 而不是 len：len 返回字节数，
// 一个中文字占 3 字节，用 len 判断会把 667 字的中文误判为"超长"。
func validateQuestion(q string) error {
	if q == "" {
		return errors.New("问题不能为空")
	}
	if n := utf8.RuneCountInString(q); n > maxQuestionChars {
		return fmt.Errorf("问题过长：%d 字符，上限 %d", n, maxQuestionChars)
	}
	return nil
}

// charLen 返回字符数（不是字节数），用于统计与日志。
func charLen(s string) int {
	return utf8.RuneCountInString(s)
}
