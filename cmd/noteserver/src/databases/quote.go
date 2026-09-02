package databases

import (
	"fmt"
	"strings"
)

// quoteLiteral 把一个值包成 SQL 字符串字面量。
//
// 为什么需要它：Norm 的 QueryBuilder.Where(cond string) 是把 cond 原样拼进
// SELECT 语句的，没有占位符、没有参数绑定。也就是说这一层只要拼错一次，
// 就是一个注入点——不像 database/sql 那样有 ? 兜着。
//
// 调用方（security.ValidPhone）已经用正则把 UID 限死成 11 位数字了，
// 但那是"另一个包里的另一条规则"。这里再挡一道：出现任何非白名单字符就
// 直接拒绝而不是转义。转义写对很难，白名单写错很难——选后者。
func quoteLiteral(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("查询条件的值不能为空")
	}
	if len(s) > 64 {
		return "", fmt.Errorf("查询条件的值过长: %d", len(s))
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '-', r == '_':
		default:
			return "", fmt.Errorf("查询条件的值含有不允许的字符: %q", r)
		}
	}
	return "'" + s + "'", nil
}

// eqCond 生成 `col`='value' 形式的条件。
func eqCond(col, val string) (string, error) {
	lit, err := quoteLiteral(val)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteByte('`')
	sb.WriteString(col)
	sb.WriteString("`=")
	sb.WriteString(lit)
	return sb.String(), nil
}
