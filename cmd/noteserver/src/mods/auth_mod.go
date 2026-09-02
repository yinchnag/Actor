// Package mods 放 actor 模块。
//
// 每个模块都是一个 actor.ModObj[*T] 的宿主：公有方法由框架反射登记，
// 由 service 包里**生成的门面**跨 goroutine 调用（见 auth_export.go）。
// 业务代码不该再出现 ModInvoke("AuthMod", "Register", ...) 这种字符串字面量——
// 方法名写错编译器不管，运行期才报 module not found。
//
// 本包遵循仓库的模块规范，`gercmd check auth cmd/noteserver/src` 能验：
//
//  1. struct 内嵌 actor.ModObj[*自己]；
//  2. 构造函数返回 actor.IModule；
//  3. 要生成门面的方法带 export: 标记。
//
// 还有两条规范之外、但这个项目必须守住的纪律：
//
// **模块方法里不做 CPU 密集的活，也不做慢 IO**。方法跑在 actor 的事件循环上，
// 一次调用有多慢，这个 actor 后面排队的请求就等多久。所以 bcrypt 在 security 包里
// 由 HTTP 协程算，这里只碰存储；而存储调用必须显著快于框架写死的 3 秒任务超时——
// Norm 的读写不接受 context，唯一的超时闸门是 data/orm.json 里 DSN 上的
// timeout/readTimeout/writeTimeout，配置文件里已经写成 2s。
//
// **参数和返回值不能用本包声明的类型**。门面生成在 service 包，引用 mods 里的
// 类型在 GameSvr 那种布局下会成环，所以 gercmd 一律拒绝；跨层的值类型统一放
// contract 包。这条约束顺带把签名逼简单了——原先那一堆 XxxArgs/XxxResult
// 包装类型只是为了迁就 ModInvoke 的 any，去掉之后签名反而更好读。
package mods

import (
	"errors"
	"log"
	"time"

	"noteserver/src/contract"

	"actor"
)

//go:generate go -C ../../../.. run ./cmd/gercmd gen -force -tmpl cmd/noteserver/templates/shard_export.tmpl -recv Hub cmd/noteserver/src/mods/auth_mod.go cmd/noteserver/src/service

// AuthMod 管账号。它被挂在按手机号分片的 auth actor 上。
//
// 为什么注册要走 actor：注册是"先查重、再建号"两步，中间有窗口。
// 按手机号哈希分片之后，同一个号码永远落到同一个 actor，两步天然串行，
// 不必依赖数据库的唯一约束——这一点在换成 Norm 之后尤其重要，
// 因为 Norm 的异步 Save 根本没有唯一冲突可以报（见 databases/account.go）。
type AuthMod struct {
	actor.ModObj[*AuthMod]
	accounts contract.IAccountStore
}

// NewAuthMod 建模块。
//
// 返回 actor.IModule 而不是 *AuthMod 是规范要求：模块只该通过 AddModule 交给
// loader，返回具体类型就给了调用方"绕过 actor 直接调方法"的机会——那样改的是
// 别人事件循环上的状态，锁全白加了。
//
// Init 靠字段偏移反推宿主指针，此后这个结构体不能再被拷贝——
// 拷贝出来的那份里，ModObj 记的仍然是原对象的地址。
func NewAuthMod(accounts contract.IAccountStore) actor.IModule {
	m := &AuthMod{accounts: accounts}
	m.Init()
	return m
}

// Register 查重并建号，返回账号 ID。手机号已存在时返回 contract.ErrPhoneTaken。
//
// passwordHash 由调用方（HTTP 层）预先算好，见包注释。
//
//	export: AuthRegister
func (that *AuthMod) Register(uid string, passwordHash string) (string, error) {
	info, err := that.accounts.Create(uid, passwordHash)
	if err != nil {
		return "", err
	}
	return info.UID, nil
}

// Lookup 取出账号，供 HTTP 层比对密码。
//
// 哈希跟着 AccountInfo 一起离开 actor，只是在同一个进程内传递，
// HTTP 层比对完就丢；换来的是 bcrypt 那几十毫秒不占用事件循环。
//
//	export: AuthLookup
func (that *AuthMod) Lookup(uid string) (contract.AccountInfo, error) {
	return that.accounts.Find(uid)
}

// TouchLogin 记一次登录时间。
//
// 没有返回值，因此是一次**投递即忘**的调用：HTTP 层不等它完成，
// 登录应答不会被这次写拖慢。
//
// 代价是失败没有调用方能接，所以只能在这里自己消化掉。注意**不能 panic**：
// 框架 handleTask 里的 recover 在上报 PhaseInvoke 之后会顺手 Close 掉整个
// actor，为了一次记录登录时间的失败干掉一整个分片，代价完全不成比例。
// 真正会被上报到 DiscardedErrorHandler 的是投递层面的失败（队列满、actor 已关），
// 那类失败本来也不是这里能处理的。
//
//	export: AuthTouchLogin
func (that *AuthMod) TouchLogin(uid string, at time.Time) {
	err := that.accounts.TouchLogin(uid, at)
	switch {
	case err == nil:
	case errors.Is(err, contract.ErrAccountNotFound):
		// 账号刚验过密码，这里查不到只可能是并发注销，不值得记一条错误日志
	default:
		log.Printf("[AuthMod] 记录 %s 的登录时间失败: %v", uid, err)
	}
}
