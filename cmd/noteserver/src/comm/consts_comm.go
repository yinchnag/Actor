// Package comm 放整个项目都能用的数据源：常量，以及跨模块传递的 Snap 值类型。
//
// 两条硬规矩：
//
//  1. **本包不引入任何其他内部包。** 它是依赖图的最底层，谁都能用它、
//     它不用任何人。一旦它开始 import contract 或 mods，分层就塌了。
//  2. **struct 一律以 Snap 结尾**，常量则无命名要求。Snap 是个标记：
//     带它的类型才可以跨模块传递。AMod 需要 BMod 的数据时，BMod 只能
//     返回 **Snap——它自己 **_type.go 里的类型属于它自己，别人不该认识。
//
// 常量部分还有个共同点：它们是**程序行为**而不是部署配置。
// 部署配置（监听地址、数据库地址）在 data/ 下的 json 里，改了要重启；
// 这里的常量改了要重新编译——这个区别本身就是一道防线，
// 免得有人把"笔记最多多少字"这种业务规则塞进运维的配置文件里。
package comm

import "time"

const (
	// MaxBodyBytes 请求体上限，防止有人拿超大 body 打内存。
	//
	// 它留在这里而不是归给某个功能：所有路由共用同一份 web.Router 选项
	// （见 bases.RouterOpts），这个上限对每个接口都成立。
	//
	// 256KB 这个数不是随手取的整数，它对齐的是 net/http 的 maxPostHandlerReadBytes：
	// handler 提前拒掉请求、请求体还没读完时，服务器会替你把剩下的读掉丢弃
	// （"drain"），好让这条 keep-alive 连接停在一个干净的消息边界上、能继续复用；
	// 但 drain 是被害者出钱，所以有上限——剩余超过 256KB 就直接断连接。
	//
	// 把上限压到正好等于那个额度，中间那段"能收下但拒了就得断连"的灰区就没了：
	//
	//	≤256KB  收下。就算解析失败也只丢一个请求，连接照常复用。
	//	>256KB  MaxBytesReader 当场掐断，服务器不会为一个注定被拒的请求
	//	        一路缓冲到 1MB。
	MaxBodyBytes = 256 << 10

	// UserIdleTimeout 用户 actor 空闲多久后回收。
	// 在线用户数决定 goroutine 数，不回收的话每个登录过的用户都会留一条协程。
	//
	// 它对**所有** **_mod.go 模块都成立（笔记、信箱……），所以留在这里。
	UserIdleTimeout = 5 * time.Minute

	// JanitorInterval 空闲回收的巡检间隔。同上，与具体功能无关。
	JanitorInterval = 30 * time.Second
)
