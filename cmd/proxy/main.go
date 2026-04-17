// proxy 是一个独立的 API 反向代理服务，用于转发 OpenAI/Anthropic 请求并记录完整日志。
//
// 用法:
//
//	go run ./cmd/proxy [选项]
//
// 环境变量:
//
//	PROXY_ADDR            监听地址（默认 :9090）
//	OPENAI_BASE_URL       OpenAI 上游地址（默认 https://api.openai.com/v1）
//	OPENAI_API_KEY        OpenAI API Key
//	ANTHROPIC_BASE_URL    Anthropic 上游地址（默认 https://api.anthropic.com）
//	ANTHROPIC_API_KEY     Anthropic API Key
//	PROXY_LOG_FILE        日志文件路径（JSONL 事件流格式，为空则使用内存存储）
//	PROXY_MAX_INLINE_BODY 内联 body 大小阈值（字节，默认 4096，超过则写入 bodies/ 目录）
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/millken/deepai/pkg/proxy"
)

func main() {
	addr := flag.String("addr", envOr("PROXY_ADDR", ":9090"), "listen address")
	openaiURL := flag.String("openai-url", envOr("OPENAI_BASE_URL", "https://api.openai.com/v1"), "OpenAI upstream base URL")
	openaiKey := flag.String("openai-key", envOr("OPENAI_API_KEY", ""), "OpenAI API key")
	anthropicURL := flag.String("anthropic-url", envOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com"), "Anthropic upstream base URL")
	anthropicKey := flag.String("anthropic-key", envOr("ANTHROPIC_API_KEY", ""), "Anthropic API key")
	logFile := flag.String("log-file", envOr("PROXY_LOG_FILE", ""), "log file path (JSONL event stream, empty = memory store)")
	maxInlineBody := flag.Int("max-inline-body", envInt("PROXY_MAX_INLINE_BODY", 4096), "max body size to inline in JSONL (bytes)")
	flag.Parse()

	if *openaiKey == "" && *anthropicKey == "" {
		slog.Error("至少需要设置 OPENAI_API_KEY 或 ANTHROPIC_API_KEY")
		os.Exit(1)
	}

	p, err := proxy.NewProxy(slog.Default(), proxy.Config{
		Addr: *addr,
		OpenAI: proxy.UpstreamConfig{
			BaseURL: *openaiURL,
			APIKey:  *openaiKey,
		},
		Anthropic: proxy.UpstreamConfig{
			BaseURL: *anthropicURL,
			APIKey:  *anthropicKey,
		},
	})
	if err != nil {
		slog.Error("create proxy", "err", err)
		os.Exit(1)
	}

	var storeDesc string
	if *logFile != "" {
		store, err := proxy.NewFileEventStore(proxy.FileEventStoreConfig{
			Path:              *logFile,
			MaxInlineBodySize: *maxInlineBody,
		})
		if err != nil {
			slog.Error("create file event store", "err", err)
			os.Exit(1)
		}
		defer store.Close()
		p.WithStore(store)
		storeDesc = "file: " + *logFile
	} else {
		storeDesc = "memory"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := p.ListenAndServe(); err != nil {
			slog.Error("proxy server error", "err", err)
		}
	}()

	fmt.Fprintf(os.Stderr, `
╔══════════════════════════════════════════════╗
║  DeepAI Proxy Server                        ║
║  Listen: %s                          ║
║  OpenAI:   %s
║  Anthropic: %s
║  Log Store: %s
╚══════════════════════════════════════════════╝
`, *addr, *openaiURL, *anthropicURL, storeDesc)

	<-ctx.Done()
	slog.Info("shutting down...")
	if err := p.Shutdown(context.Background()); err != nil {
		slog.Error("shutdown", "err", err)
		os.Exit(1)
	}
	slog.Info("proxy stopped")
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
