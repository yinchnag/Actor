// Package security 放认证相关的纯计算：密码哈希、令牌生成、输入格式校验。
//
// 单独成包不是为了分层好看，而是因为这些函数有一条共同的部署纪律：
// **它们绝不能在 actor 的事件循环里跑**。bcrypt 是 50~70ms 的纯 CPU 开销，
// 放进事件循环会把整个分片钉死那么久，几十个并发登录就打满了。
// 把它们收在一个包里，"谁在调用 security.*"就成了一个可以一眼扫出来的问题。
package security

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"

	"noteserver/src/comm"

	"golang.org/x/crypto/bcrypt"
)

// phonePattern 中国大陆手机号。
//
// 需求只要求"必须是手机号"、不要求短信验证，所以这里只做格式校验。
// 要支持国际号码就改这个正则——但改之前先看 databases/quote.go：
// Norm 的查询条件是拼进 SQL 的字符串，手机号的字符集是那一层安全性的前提。
var phonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)

// dummyHash 是一个固定的 bcrypt 哈希，用于账号不存在时的假比对。
//
// 不这么做的话，"账号不存在"会立刻返回，而"密码错误"要花几十毫秒算 bcrypt，
// 攻击者靠响应时间就能枚举出哪些手机号注册过。
var dummyHash []byte

func init() {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)
	if err != nil {
		panic(err) // 只在 bcrypt 参数非法时发生，属于编程错误
	}
	dummyHash = h
}

// ValidPhone 判手机号格式。
func ValidPhone(phone string) bool { return phonePattern.MatchString(phone) }

// CheckPassword 校验密码长度，不合规时返回可直接展示给用户的原因。
func CheckPassword(pw string) (string, bool) {
	if len([]rune(pw)) < comm.MinPasswordLen {
		return fmt.Sprintf("密码至少 %d 位", comm.MinPasswordLen), false
	}
	if len(pw) > comm.MaxPasswordBytes {
		return fmt.Sprintf("密码过长（最多 %d 字节）", comm.MaxPasswordBytes), false
	}
	return "", true
}

// HashPassword 算 bcrypt 哈希。几十毫秒的纯 CPU 活，只在 HTTP 协程上调用。
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// ComparePassword 比对密码。
//
// found 为 false 时用内置的假哈希走一遍完整的 bcrypt 计算，
// 让"账号不存在"和"密码错误"两条路径耗时一致——否则响应时间本身
// 就是一个"这个号注册过没有"的探测接口。
func ComparePassword(hash, pw string, found bool) bool {
	stored := dummyHash
	if found {
		stored = []byte(hash)
	}
	ok := bcrypt.CompareHashAndPassword(stored, []byte(pw)) == nil
	return ok && found
}

// NewToken 生成 32 字节随机会话令牌的十六进制表示。
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
