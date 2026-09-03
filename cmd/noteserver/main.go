// noteserver 是 actor 框架的一个完整示例：手机号注册、登录、上传笔记、获取笔记。
//
// 它是一个**独立的 Go 模块**（见同目录 go.mod），通过 replace 指向上两级的
// actor 框架与本地的 Norm ORM。这样做是为了让示例能有自己的依赖
// （gin、Norm）而不把它们塞进框架本身的 go.mod——框架应当保持零业务依赖。
//
// 目录结构参考 roleSvr：main 只做装配，逻辑分层在 src/ 下。
package main

import (
	"flag"
	"path/filepath"

	"noteserver/src/bases"
	"noteserver/src/config"
	"noteserver/src/databases"
	"noteserver/src/logs"
	"noteserver/src/middleware"
	"noteserver/src/router/auth"
	"noteserver/src/router/health"
	"noteserver/src/router/mail"
	"noteserver/src/router/note"
	"noteserver/src/service"

	"github.com/norm/orm"
)

func main() {
	dataDir := flag.String("data", "data", "配置目录，内含 server.json 与 orm.json")
	flag.Parse()

	cfg, err := config.LoadServer(filepath.Join(*dataDir, "server.json"))
	if err != nil {
		logs.Fatalf("配置有问题: %v", err)
	}

	// 初始化 MySQL / Redis 连接池，必须在任何 ORM 操作之前调用一次。
	// databases 包里每个 store 的构造函数都会触发 AutoMigrate，
	// 那是 ORM 操作，所以顺序不能反。
	if err := orm.InitPool(filepath.Join(*dataDir, "orm.json")); err != nil {
		logs.Fatalf("连接存储失败: %v", err)
	}
	// 优雅退出：进程结束前把异步队列里未落盘的数据刷进 MySQL
	defer orm.Shutdown()

	// 异步存档失败是这套 ORM 里唯一没有调用栈可以返回错误的地方。
	// 不接管的话，一条笔记写不进 MySQL 会彻底静默——Redis 里还有，
	// 等 TTL 一到就真的没了。
	orm.SetArchiveErrorHandler(func(ev orm.ArchiveError) {
		if ev.Dropped {
			// 重试已用尽，这份数据在系统里只剩这一条日志
			logs.Errorf("[存档丢失] %s | payload=%s", ev.Error(), ev.PayloadJSON())
			return
		}
		logs.Warnf("[存档失败，将重试] %s", ev.Error())
	})

	hub := service.NewHub(service.Deps{
		Accounts: databases.NewAccountStore(),
		Notes:    databases.NewNoteStore(),
		Mails:    databases.NewMailStore(),
		Sessions: databases.NewSessionStore(),
	})
	defer hub.Close()

	bases.Setup(cfg.GinMode)
	// 路径与 HTTP 方法都写在各自的请求类型上，这里只决定"挂在哪个分组、
	// 套哪些中间件"。具体有哪几条接口，看各路由包里的方法签名。
	// 两个分组的区别只有中间件，路径都由各请求类型上的 path tag 给出
	userGroup := bases.V1.Group("", middleware.Auth(hub.Sessions()))

	routers := []interface{ Routes() []string }{
		health.New(bases.R, hub), // 挂在引擎根上，不带 /api 前缀
		auth.New(bases.V1, hub),
		// 笔记接口整组套鉴权：两个接口都要求登录，套在分组上不会漏。
		// 分组不加前缀，路径由 note 包请求类型上的 path tag 给出。
		note.New(userGroup, hub),
		// 邮件分两种身份：拉取/领取归用户，下发归运维。
		// 运维那条能凭空发放道具，所以走自己的令牌，不用用户的 Bearer。
		mail.NewUser(userGroup, hub),
		mail.NewOps(bases.V1.Group("", middleware.OpsAuth(cfg.OpsToken)), hub),
	}
	// 把自动注册的结果打出来。路由不再写在代码里，启动日志就成了唯一
	// 一处能一眼看全"到底开了哪些接口"的地方，值得占几行。
	for _, r := range routers {
		for _, line := range r.Routes() {
			logs.Infof("路由 %s", line)
		}
	}

	// 先停 HTTP 再关 actor：让在途请求把手上的 actor 调用跑完并释放。
	// 顺序反了的话在途请求会拿到一堆 actor is closed。
	if err := bases.Run(cfg.Listen(), bases.R, hub.Close); err != nil {
		logs.Fatalf("监听失败: %v", err)
	}
	logs.Infof("已退出")
}
