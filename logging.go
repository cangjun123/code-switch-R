package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/daodao97/xgo/xlog"
)

// logLevelEnv 覆盖默认日志级别（debug / info / warn / error）
const logLevelEnv = "CODE_SWITCH_LOG_LEVEL"

// maxLoggedAttrBytes 单个日志字段的最大长度。xrequest 在上游失败时（WARN）会记录完整的
// curl 命令（含请求体）和响应体，必须截断，避免对话内容/图片 base64 写进系统日志。
const maxLoggedAttrBytes = 1024

// configureLogging 替换 xlog 的默认 logger。xlog 默认 DEBUG 级别，xrequest 会把每个上游请求
// 连同完整请求体以 curl 形式打印、xdb 会打印每条 SQL，生产环境下每小时可写入上百 MB 日志。
func configureLogging() {
	level := parseLogLevel(os.Getenv(logLevelEnv))
	base := xlog.StdoutTextPretty(xlog.WithLevel(level))
	xlog.SetLogger(slog.New(truncatingHandler{base.Handler()}))
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// truncatingHandler 在交给底层 handler 前截断过长的字段值
// （xlog 的 PrettyHandler 自行格式化字段，不支持 ReplaceAttr）。
type truncatingHandler struct{ slog.Handler }

func (h truncatingHandler) Handle(ctx context.Context, record slog.Record) error {
	truncated := slog.NewRecord(record.Time, record.Level, truncateLogText(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		truncated.AddAttrs(truncateLogAttr(attr))
		return true
	})
	return h.Handler.Handle(ctx, truncated)
}

func (h truncatingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	for i := range attrs {
		attrs[i] = truncateLogAttr(attrs[i])
	}
	return truncatingHandler{h.Handler.WithAttrs(attrs)}
}

func (h truncatingHandler) WithGroup(name string) slog.Handler {
	return truncatingHandler{h.Handler.WithGroup(name)}
}

func truncateLogAttr(attr slog.Attr) slog.Attr {
	value := attr.Value.Resolve()
	var text string
	switch value.Kind() {
	case slog.KindString:
		text = value.String()
	case slog.KindAny:
		switch v := value.Any().(type) {
		case string:
			text = v
		case []byte:
			text = string(v)
		case error:
			text = v.Error()
		case fmt.Stringer:
			text = v.String()
		default:
			return attr
		}
	default:
		return attr
	}
	if len(text) <= maxLoggedAttrBytes {
		return attr
	}
	return slog.String(attr.Key, truncateLogText(text))
}

func truncateLogText(text string) string {
	if len(text) <= maxLoggedAttrBytes {
		return text
	}
	return fmt.Sprintf("%s...(truncated, %d bytes)", text[:maxLoggedAttrBytes], len(text))
}
