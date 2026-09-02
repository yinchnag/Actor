package security

import (
	"strings"
	"testing"

	"noteserver/src/comm"
)

// TestValidPhone 手机号格式校验。
//
// 它同时还是 databases 那层 SQL 安全的前提：quoteLiteral 只放行
// 数字、字母、连字符和下划线，而这里保证进到存储层的 UID 只有 11 位数字。
func TestValidPhone(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"13800138000", true},
		{"19912345678", true},
		{"12800138000", false}, // 第二位必须是 3-9
		{"23800138000", false}, // 必须以 1 开头
		{"1380013800", false},  // 位数不足
		{"138001380000", false},
		{"+8613800138000", false},
		{"a@b.com", false},
		{"", false},
		{"13800138000' OR '1'='1", false},
	} {
		if got := ValidPhone(c.in); got != c.want {
			t.Errorf("ValidPhone(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestCheckPassword 长度约束的两端。
func TestCheckPassword(t *testing.T) {
	if _, ok := CheckPassword(strings.Repeat("a", comm.MinPasswordLen-1)); ok {
		t.Error("短一位的密码应当被拒绝")
	}
	if _, ok := CheckPassword(strings.Repeat("a", comm.MinPasswordLen)); !ok {
		t.Error("刚好到下限的密码应当通过")
	}
	if _, ok := CheckPassword(strings.Repeat("a", comm.MaxPasswordBytes)); !ok {
		t.Error("刚好 72 字节的密码应当通过")
	}
	// bcrypt 只认前 72 字节，多出来的部分会被静默忽略——必须显式拒绝
	if _, ok := CheckPassword(strings.Repeat("a", comm.MaxPasswordBytes+1)); ok {
		t.Error("超过 72 字节的密码应当被拒绝")
	}
	// 长度按字符数算下限、按字节数算上限：8 个汉字是 24 字节，应当通过
	if _, ok := CheckPassword("密码密码密码密码"); !ok {
		t.Error("8 个汉字的密码应当通过")
	}
}

// TestComparePasswordTimingPath 账号不存在时也要走完一次真实的 bcrypt。
//
// 这条测的是"结果对不对"；耗时是否真的对齐没法在单元测试里稳定断言，
// 但只要 found=false 仍然进了 CompareHashAndPassword，时间就是可比的。
func TestComparePassword(t *testing.T) {
	hash, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatalf("算哈希失败: %v", err)
	}
	if !ComparePassword(hash, "correct-horse", true) {
		t.Error("正确密码应当通过")
	}
	if ComparePassword(hash, "wrong-horse", true) {
		t.Error("错误密码不该通过")
	}
	// 账号不存在：即使把空哈希传进来也必须返回 false，且不能 panic
	if ComparePassword("", "any-password", false) {
		t.Error("账号不存在时不该通过")
	}
}

// TestNewToken 令牌是 32 字节随机数的十六进制，长度固定 64，且不重复。
func TestNewToken(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("生成令牌失败: %v", err)
		}
		if len(tok) != 64 {
			t.Fatalf("令牌长度应当是 64, got %d", len(tok))
		}
		if seen[tok] {
			t.Fatal("生成了重复的令牌")
		}
		seen[tok] = true
	}
}
