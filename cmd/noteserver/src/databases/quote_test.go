package databases

import "testing"

// TestQuoteLiteralRejectsInjection 是本包里最该有测试的一个函数。
//
// Norm 的 Where 没有占位符，条件字符串是原样拼进 SELECT 的——这里放过一个
// 引号，就是一个注入点。所以宁可用白名单拒绝，也不做转义。
func TestQuoteLiteralRejectsInjection(t *testing.T) {
	for _, s := range []string{
		"13800138000' OR '1'='1",
		"13800138000'",
		`13800138000"`,
		"13800138000;DROP TABLE note",
		"13800138000 OR 1=1", // 空格也不放过：合法 UID 里没有空格
		"13800138000\\",
		"13800138000`",
		"13800138000	",
		"手机号", // 非 ASCII
		"",    // 空
	} {
		if _, err := quoteLiteral(s); err == nil {
			t.Errorf("quoteLiteral(%q) 应当被拒绝", s)
		}
	}
}

// TestQuoteLiteralAllows 白名单里的字符必须放行，否则正常账号会被误伤。
func TestQuoteLiteralAllows(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"13800138000", "'13800138000'"},
		// 笔记主键的形态：uid-毫秒-随机后缀
		{"13800138000-1756400000000-a1b2c3d4e5f6", "'13800138000-1756400000000-a1b2c3d4e5f6'"},
		{"abc_DEF-123", "'abc_DEF-123'"},
	} {
		got, err := quoteLiteral(c.in)
		if err != nil {
			t.Fatalf("quoteLiteral(%q) 意外失败: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("quoteLiteral(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestQuoteLiteralLength 超长的值直接拒掉。
//
// 上限对齐表里最长的那个主键列（note_id VARCHAR(64)）：比列还长的值
// 不可能匹配到任何行，让它走到数据库只是白跑一趟。
func TestQuoteLiteralLength(t *testing.T) {
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := quoteLiteral(string(long)); err == nil {
		t.Fatal("65 字节的值应当被拒绝")
	}
}

// TestEqCond 生成的条件必须给列名加反引号，否则遇上 SQL 关键字做列名就炸。
func TestEqCond(t *testing.T) {
	got, err := eqCond("uid", "13800138000")
	if err != nil {
		t.Fatalf("意外失败: %v", err)
	}
	if want := "`uid`='13800138000'"; got != want {
		t.Errorf("eqCond = %q, want %q", got, want)
	}
	if _, err := eqCond("uid", "x' OR '1'='1"); err == nil {
		t.Error("注入串应当在 eqCond 这一层就被拒绝")
	}
}
