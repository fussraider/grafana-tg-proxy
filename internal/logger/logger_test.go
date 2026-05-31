package logger

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "http://api.telegram.org/bot123456:ABC-DEF1234ghIkl-zyx/sendMessage",
			expected: "http://api.telegram.org/bot****:****/sendMessage",
		},
		{
			input:    "failed with token bot987654:XYZ987abc for channel 12",
			expected: "failed with token bot****:**** for channel 12",
		},
		{
			input:    "normal log message without tokens",
			expected: "normal log message without tokens",
		},
		{
			input:    "bot123:abc",
			expected: "bot****:****",
		},
		{
			input:    "Proxy loaded: socks5://admin:secretPass@192.168.1.1:1080",
			expected: "Proxy loaded: socks5://****:****@192.168.1.1:1080",
		},
		{
			input:    "socks5://admin:secretPass@192.168.1.1:1080",
			expected: "socks5://****:****@192.168.1.1:1080",
		},
	}

	for _, tc := range tests {
		got := Redact(tc.input)
		if got != tc.expected {
			t.Errorf("Redact(%q) = %q; expected %q", tc.input, got, tc.expected)
		}
	}
}

func TestLoggerFormat(t *testing.T) {
	var buf bytes.Buffer
	l := &Logger{
		scope: "test",
		out:   &buf,
	}

	Setup("debug", "json", false)
	l.Debug("token test bot12345:abc")

	outStr := buf.String()
	if !strings.Contains(outStr, `"level":"debug"`) {
		t.Errorf("expected level debug in output, got: %s", outStr)
	}
	if !strings.Contains(outStr, `"scope":"test"`) {
		t.Errorf("expected scope test in output, got: %s", outStr)
	}
	if !strings.Contains(outStr, "bot****:****") {
		t.Errorf("expected redacted token in output, got: %s", outStr)
	}
}
