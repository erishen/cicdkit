package config

import (
	"encoding/hex"
	"testing"
)

func TestGenerateToken(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken 报错: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("Token 长度应为 64（32 字节 hex），得到 %d", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("Token 不是合法 hex: %v", err)
	}
	// 两次生成应不同（随机性）。
	b, _ := GenerateToken()
	if a == b {
		t.Fatalf("两次生成的 Token 不应相同")
	}
}

func TestIsTrue(t *testing.T) {
	cases := map[string]bool{
		"1": true, "true": true, "TRUE": true, "True": true,
		"yes": true, "on": true, "y": true, "t": true,
		"": false, "0": false, "false": false, "no": false, "random": false,
	}
	for in, want := range cases {
		if got := isTrue(in); got != want {
			t.Fatalf("isTrue(%q) = %v, want %v", in, got, want)
		}
	}
}
