// Package logs 是全项目唯一的日志出口，底层是 GCore 的 logger。
//
// 为什么包一层而不是各处直接用 GCore
// ---------------------------------
//
//   - GCore 的包名是 utils，直接散到几十个文件里，import 行全是
//     `utils "github.com/yinchnag/GCore"`，读代码的人根本看不出那是日志库。
//   - 换实现时只改这一个文件。日志库是最容易被换掉的基础设施之一。
//   - 等级语义可以在这里一次讲清楚（见下），而不是指望每个人都去读 GCore 的源码。
//
// 等级怎么选
// ---------
//
// 这不是风格问题，选错有实际代价：**GCore 的 Errorf 会往日志里附一整份
// debug.Stack()**，而且配置里 assert 打开时还会 panic。把预期内的失败写成
// Errorf，错误日志会被几十行栈淹没，真正的 bug 反而找不着。
//
//	Debugf  排障用的细节。线上通常关掉。
//	Infof   生命周期事件：启动、监听、路由表、退出。
//	Warnf   **预期内的失败**——存储抖动、会话读不到、调用超时、用户输错。
//	        它们不代表代码有问题，只代表这次没成。不带栈。
//	Errorf  **说明有 bug**——不该发生却发生了。带完整调用栈，值得有人去看。
//	Fatalf  启动期无法继续，记完就退出。只该出现在 main 里。
//
// 一句话判据：**这条日志出现时，需不需要有人去改代码？** 需要就 Errorf，
// 不需要就 Warnf。
package logs

import (
	utils "github.com/yinchnag/GCore"
)

// Debugf 排障细节。
func Debugf(format string, args ...any) { utils.Logger.Debugf(format, args...) }

// Infof 生命周期事件。
func Infof(format string, args ...any) { utils.Logger.Infof(format, args...) }

// Warnf 预期内的失败。不带调用栈——它们不代表代码有问题。
func Warnf(format string, args ...any) { utils.Logger.Warnf(format, args...) }

// Errorf 说明有 bug。GCore 会附完整调用栈，所以别拿它记预期内的失败。
func Errorf(format string, args ...any) { utils.Logger.Errorf(format, args...) }

// Fatalf 记完就退出进程。只该出现在 main 的启动路径上——
// 服务已经跑起来之后杀进程，比让那一个请求失败糟糕得多。
func Fatalf(format string, args ...any) { utils.Logger.Fatalf(format, args...) }
