package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 保存服务的全部可配置项。
//
// 设计原则：代码里不出现任何密钥。所有敏感项从环境变量读取，
// 找不到时回退到本机已有的凭据文件，方便本地开发又不把密钥写进仓库。
type Config struct {
	Addr       string        // 监听地址，如 ":8080"
	APIURL     string        // LLM 接口地址
	APIKey     string        // 鉴权密钥
	Model      string        // 模型名
	MaxTokens  int           // 单次回复上限
	Timeout    time.Duration // 单次请求超时
	MaxRetries int           // 最大尝试次数（含首次）
	SystemRule string        // 系统提示词
}

// LoadConfig 从环境变量装配配置；缺关键项时返回错误而不是 panic，
// 让调用方决定怎么提示用户。
func LoadConfig() (*Config, error) {
	cfg := &Config{
		Addr:       envString("ADDR", ":8080"),
		APIURL:     envString("LLM_API_URL", "https://www.geek2api.com/v1/responses"),
		Model:      envString("LLM_MODEL", "deepseek-v4.1-flash"),
		MaxTokens:  envInt("LLM_MAX_TOKENS", 1024),
		Timeout:    time.Duration(envInt("LLM_TIMEOUT_SECONDS", 60)) * time.Second,
		MaxRetries: envInt("LLM_MAX_RETRIES", 3),
		SystemRule: envString("LLM_SYSTEM_PROMPT", "你是一个简洁、准确的中文助手。回答不超过三句话。"),
	}

	key := strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("GEEK1_API_KEY"))
	}
	if key == "" {
		// 回退：读本机 DSH 凭据文件，避免把密钥写进代码或 .env
		if k, err := readKeyFromCredentials(); err == nil {
			key = k
		}
	}
	if key == "" {
		return nil, fmt.Errorf(
			"未找到 API Key：请设置环境变量 LLM_API_KEY，或确认 %s 中包含 GEEK1_API_KEY",
			credentialsPath(),
		)
	}
	cfg.APIKey = key

	if cfg.MaxRetries < 1 {
		cfg.MaxRetries = 1
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	return cfg, nil
}

// credentialsPath 返回 DSH 凭据文件路径。
func credentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".dsh", ".credentials.yaml")
}

// readKeyFromCredentials 从凭据文件里按行找 GEEK1_API_KEY。
// 只做简单的文本匹配，不引入 YAML 依赖。
func readKeyFromCredentials() (string, error) {
	path := credentialsPath()
	if path == "" {
		return "", fmt.Errorf("无法定位用户目录")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "GEEK1_API_KEY") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if key := strings.TrimSpace(parts[1]); key != "" {
			return key, nil
		}
	}
	return "", fmt.Errorf("凭据文件中未找到 GEEK1_API_KEY")
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
