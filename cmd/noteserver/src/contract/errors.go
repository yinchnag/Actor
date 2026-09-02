// Package contract 只放接口，以及与接口直接相关的东西（这里是它们返回的错误哨兵）。
//
// 两条硬规矩：
//
//  1. **不放普通对象。** 跨模块传的值类型一律去 comm 定义成 **Snap，
//     这里只描述"谁能做什么"，不描述"数据长什么样"。
//  2. **只能引入 comm。** 它在依赖图上紧挨着 comm，再往上就该是 databases 了。
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
