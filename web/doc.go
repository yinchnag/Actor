// Package web 是一套基于 gin 的自动路由：路由结构体内嵌 Router[*自己]，
// 公有方法被反射扫成 HTTP 接口，动词与路径由请求结构体上的标记声明。
//
//	type Auth struct {
//	    web.Router[*Auth]
//	}
//
//	type LoginRequest struct {
//	    web.POST                        // 动词；不写 path tag 就按方法名推出 /login
//	    Phone string `json:"phone"`
//	}
//
//	func (that *Auth) Login(req *LoginRequest, ctx *gin.Context) { ... }
//
//	a := &Auth{}
//	a.Init(engine.Group("/api"))       // → 注册 POST /api/login
//
// 全程没有一处 group.POST("/login", ...)。加一个接口 = 加一个方法 + 一个请求类型。
//
// # 为什么是独立模块
//
// 它和同仓库的 actor 框架是平级的两件东西，**互不依赖**——本包一行 actor 的代码
// 都没引用，共用的只是 CRTP 这个手法（actor.ModObj[T] 解决"跨 goroutine 按方法名
// 调用模块"，这里解决"按方法签名生成 HTTP 路由"）。
//
// 拆成独立模块而不是塞进 actor，是为了 gin：它会拖进 16 个传递依赖
// （sonic、validator、go-playground/*、golang.org/x/net、protobuf…）。
// 只想要 actor 并发模型、根本不做 HTTP 的项目不该为此买单。
package web
