package bases

import (
	"noteserver/src/comm"

	"web"
)

// RouterOpts 是本服务传给 web.Router 的选项。
//
// web 那个包只管"怎么把方法扫成路由"，剩下两件事是**业务决定**，它不替使用者做：
//
//   - 请求体多大算大。这里给 comm.MaxBodyBytes，那边有为什么是 256KB 的账。
//   - 错误应答长什么样。这里给 Fail，于是 web 内部的绑定失败与本服务 handler
//     手写的错误走同一个出口，客户端看到的形状一致。
//
// 三个路由包都用它，改一处就全改了：
//
//	a.Init(group, bases.RouterOpts()...)
func RouterOpts() []web.Option {
	return []web.Option{
		web.WithMaxBodyBytes(comm.MaxBodyBytes),
		web.WithErrorWriter(Fail),
	}
}
