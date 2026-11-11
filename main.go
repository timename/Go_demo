package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[gateway] ")

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	srv := NewServer(cfg)
	mux := http.NewServeMux()
	srv.Register(mux)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           logMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// 流式响应可能持续较久，写超时不能设太短
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	// 优雅关闭：收到 Ctrl+C / SIGTERM 时先停止接收新请求，
	// 给正在处理的请求最多 10 秒收尾时间。
	idleClosed := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh

		log.Println("收到退出信号，正在关闭...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("关闭时出错: %v", err)
		}
		close(idleClosed)
	}()

	fmt.Println("──────────────────────────────────────────")
	fmt.Printf("Go LLM Gateway\n")
	fmt.Printf("  监听地址 : http://localhost%s\n", cfg.Addr)
	fmt.Printf("  模型     : %s\n", cfg.Model)
	fmt.Printf("  超时     : %v\n", cfg.Timeout)
	fmt.Printf("  最大重试 : %d 次\n", cfg.MaxRetries)
	fmt.Println("──────────────────────────────────────────")
	fmt.Printf("  健康检查 : http://localhost%s/health\n", cfg.Addr)
	fmt.Printf("  调用统计 : http://localhost%s/stats\n", cfg.Addr)
	fmt.Printf("  演示页面 : http://localhost%s/demo\n", cfg.Addr)
	fmt.Println("──────────────────────────────────────────")

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("服务启动失败: %v", err)
	}

	<-idleClosed
	log.Println("已退出")
}

// logMiddleware 记录每个请求的方法、路径、状态码与耗时。
// 这是 JD 里「日志、监控」要求的最小实现。
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%dms)", r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds())
	})
}

// statusRecorder 包一层 ResponseWriter，用来捕获实际写出的状态码。
// 不包的话拿不到 handler 内部的 WriteHeader 调用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush 转发给底层，保证流式响应能真正刷出去。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
