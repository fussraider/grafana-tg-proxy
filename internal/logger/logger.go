package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Level definitions
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Color escape codes
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
)

var (
	// Regex to match bot tokens like bot123456:ABC-DEF1234ghIkl-zyx
	tokenRegex = regexp.MustCompile(`(?i)bot\d+:[a-z0-9_\-]+`)
	// Regex to match SOCKS5 proxy credentials: socks5://username:password@
	socksAuthRegex = regexp.MustCompile(`(?i)(socks5?://)[^:/@]+:[^/@]+@`)
	
	// Global logger configuration
	globalLevel  = LevelInfo
	globalFormat = "plain"
	globalColor  = true
	mu           sync.Mutex
)

// Setup sets the global configuration for the logger.
func Setup(level, format string, color bool) {
	mu.Lock()
	defer mu.Unlock()
	globalLevel = strings.ToLower(level)
	globalFormat = strings.ToLower(format)
	globalColor = color
}

// Logger represents a scoped logger instance.
type Logger struct {
	scope string
	out   io.Writer
}

// New creates a new Logger with a given scope.
func New(scope string) *Logger {
	return &Logger{
		scope: scope,
		out:   os.Stdout,
	}
}

// WithScope returns a new logger with the specified scope.
func (l *Logger) WithScope(scope string) *Logger {
	return &Logger{
		scope: scope,
		out:   l.out,
	}
}

// Redact replaces bot tokens and proxy credentials in the string with a redacted placeholder.
func Redact(text string) string {
	t := tokenRegex.ReplaceAllString(text, "bot****:****")
	return socksAuthRegex.ReplaceAllString(t, "${1}****:****@")
}

// shouldLog returns true if the current message level is enabled.
func shouldLog(level string) bool {
	levels := map[string]int{
		LevelDebug: 0,
		LevelInfo:  1,
		LevelWarn:  2,
		LevelError: 3,
	}

	currentVal, ok := levels[globalLevel]
	if !ok {
		currentVal = 1 // default to info
	}

	msgVal, ok := levels[level]
	if !ok {
		return true
	}

	return msgVal >= currentVal
}

func (l *Logger) log(level, msg string, args ...any) {
	if !shouldLog(level) {
		return
	}

	formattedMsg := msg
	if len(args) > 0 {
		formattedMsg = fmt.Sprintf(msg, args...)
	}

	// Redact bot tokens from the message
	redactedMsg := Redact(formattedMsg)
	timestamp := time.Now()

	if globalFormat == "json" {
		l.logJSON(level, redactedMsg, timestamp)
	} else {
		l.logPlain(level, redactedMsg, timestamp)
	}
}

type jsonPayload struct {
	Level     string    `json:"level"`
	Timestamp string    `json:"timestamp"`
	Scope     string    `json:"scope,omitempty"`
	Message   string    `json:"message"`
}

func (l *Logger) logJSON(level, msg string, t time.Time) {
	payload := jsonPayload{
		Level:     level,
		Timestamp: t.UTC().Format(time.RFC3339),
		Scope:     l.scope,
		Message:   msg,
	}

	data, err := json.Marshal(payload)
	if err == nil {
		fmt.Fprintln(l.out, string(data))
	} else {
		fmt.Fprintf(l.out, `{"level":"error","timestamp":"%s","scope":"logger","message":"failed to serialize log"}`+"\n", t.UTC().Format(time.RFC3339))
	}
}

func (l *Logger) logPlain(level, msg string, t time.Time) {
	timeStr := t.Format("2006-05-31 15:04:05")
	levelStr := strings.ToUpper(level)
	scopeStr := ""
	if l.scope != "" {
		scopeStr = fmt.Sprintf("[%s] ", l.scope)
	}

	if globalColor {
		var levelColor string
		switch level {
		case LevelDebug:
			levelColor = colorPurple
		case LevelInfo:
			levelColor = colorGreen
		case LevelWarn:
			levelColor = colorYellow
		case LevelError:
			levelColor = colorRed
		default:
			levelColor = colorReset
		}
		levelStr = levelColor + fmt.Sprintf("%-5s", levelStr) + colorReset
		scopeStr = colorCyan + scopeStr + colorReset
	} else {
		levelStr = fmt.Sprintf("%-5s", levelStr)
	}

	fmt.Fprintf(l.out, "%s %s %s%s\n", timeStr, levelStr, scopeStr, msg)
}

// Debug logs a message at debug level.
func (l *Logger) Debug(msg string, args ...any) {
	l.log(LevelDebug, msg, args...)
}

// Info logs a message at info level.
func (l *Logger) Info(msg string, args ...any) {
	l.log(LevelInfo, msg, args...)
}

// Warn logs a message at warning level.
func (l *Logger) Warn(msg string, args ...any) {
	l.log(LevelWarn, msg, args...)
}

// Error logs a message at error level.
func (l *Logger) Error(msg string, args ...any) {
	l.log(LevelError, msg, args...)
}
