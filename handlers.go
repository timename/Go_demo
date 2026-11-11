package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ══════════════════════════════════════════════════════════
// 统计：满足 JD 里「日志、监控、成本与延迟统计」的要求
// ══════════════════════════════════════════════════════════

// Stats 记录服务运行期间的调用情况。
// 用互斥锁保护，因为 HTTP handler 是并发执行的。
type Stats struct {
	mu           sync.Mutex
	Total        int64 // 总请求数
	Succeeded    int64 // 成功数
	Failed       int64 // 失败数
	TotalLatency int64 // 累计耗时（毫秒）
	PromptChars  int64 // 累计输入字符数
	StartedAt    time.Time
}

func NewStats() *Stats {
	return &Stats{StartedAt: time.Now()}
}

// Record 记录一次调用的结果。
func (s *Stats) Record(ok bool, latency time.Duration, promptChars int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Total++
	if ok {
		s.Succeeded++
	} else {
		s.Failed++
	}
	s.TotalLatency += latency.Milliseconds()
	s.PromptChars += int64(promptChars)
}

// Snapshot 返回一份快照，供 JSON 输出。
func (s *Stats) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	avg := int64(0)
	if s.Total > 0 {
		avg = s.TotalLatency / s.Total
	}
	return map[string]any{
		"total":          s.Total,
		"succeeded":      s.Succeeded,
		"failed":         s.Failed,
		"avg_latency_ms": avg,
		"prompt_chars":   s.PromptChars,
		"uptime_seconds": int64(time.Since(s.StartedAt).Seconds()),
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
	mux.HandleFunc("/chat/stream", s.handleChatStream) // GET/POST 流式问答
	mux.HandleFunc("/demo", s.handleDemo)              // 前端演示页
}

// ── 根路径 ────────────────────────────────────────────────
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
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
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "请使用 POST")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	req.Question = trimSpace(req.Question)
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, "question 不能为空")
		return
	}
	if len(req.Question) > 2000 {
		writeError(w, http.StatusBadRequest, "question 过长（上限 2000 字符）")
		return
	}

	start := time.Now()
	resp, err := s.llm.Ask(r.Context(), req.Question)
	latency := time.Since(start)

	if err != nil {
		s.stats.Record(false, latency, len(req.Question))
		log.Printf("[chat] 失败 question=%q err=%v", truncate(req.Question, 50), err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.stats.Record(true, latency, len(req.Question))
	log.Printf("[chat] 成功 attempts=%d latency=%dms", resp.Attempts, resp.LatencyMS)
	writeJSON(w, http.StatusOK, resp)
}

// ── 流式问答（SSE）────────────────────────────────────────
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	question := r.URL.Query().Get("q")
	if question == "" {
		writeError(w, http.StatusBadRequest, "缺少参数 q")
		return
	}
	question = trimSpace(question)
	if len(question) > 2000 {
		writeError(w, http.StatusBadRequest, "q 过长（上限 2000 字符）")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前服务器不支持流式响应")
		return
	}

	// SSE 必须的三个头
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关闭 nginx 缓冲

	start := time.Now()
	chunks := 0

	send := func(chunk StreamChunk) {
		raw, err := json.Marshal(chunk)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", raw)
		flusher.Flush() // 关键：不 Flush 的话数据会积压在缓冲区
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	err := s.llm.AskStream(ctx, question, func(c StreamChunk) {
		if c.Type == "delta" {
			chunks++
		}
		send(c)
	})

	if err != nil {
		s.stats.Record(false, time.Since(start), len(question))
		log.Printf("[stream] 失败 err=%v", err)
		send(StreamChunk{Type: "error", Text: err.Error()})
		return
	}

	s.stats.Record(true, time.Since(start), len(question))
	log.Printf("[stream] 成功 chunks=%d latency=%dms", chunks, time.Since(start).Milliseconds())
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

// trimSpace 单独抽出，方便以后替换为更严格的清洗逻辑（如 PII 脱敏）。
func trimSpace(s string) string {
	out := make([]rune, 0, len(s))
	started := false
	spaceRun := 0
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			spaceRun++
			continue
		}
		if started && spaceRun > 0 {
			out = append(out, ' ')
		}
		spaceRun = 0
		started = true
		out = append(out, r)
	}
	return string(out)
}
