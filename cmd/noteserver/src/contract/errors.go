// Package contract 只放接口与跨层错误哨兵，不放实现。
//
// 它存在的理由是可测性：actor 模块与 HTTP 层依赖这里的接口，
// 生产环境注入 databases 包里的 Norm 实现，测试注入内存实现——
// 于是"actor 编排是否正确"这件事不需要一台 MySQL 就能验证。
// roleSvr 里 service 直接调 databases，那是业务服务器的写法；
// 本项目是示例，测试跑不起来就失去了示例的意义。
package contract

import "errors"

var (
	// ErrPhoneTaken 手机号已注册。
	ErrPhoneTaken = errors.New("手机号已被注册")
	// ErrAccountNotFound 手机号没有对应的账号。
	ErrAccountNotFound = errors.New("账号不存在")
	// ErrNoSession token 不存在或已过期。
	ErrNoSession = errors.New("会话不存在或已过期")
)
