// Package auth 是账号功能模块。
//
// 它是一个 **Mgr**（挂在服务器上）而不是 **Mod**（挂在用户身上）：判断标准是
// "没有用户时这个功能是否也需要正常运行"。登入验证显然需要——用户还没登进来，
// 谁给他挂模块？所以文件叫 auth_mgr.go，类型叫 AuthMgr，由 Hub 在**启动时**加载。
//
// 对照 mods/note：那个功能只在用户在线时有意义，所以是 note_mod.go / NoteMod，
// 用户登入成功之后才给他挂上。
package auth

import (
	"errors"
	"time"

	"noteserver/src/comm"
	"noteserver/src/contract"
	"noteserver/src/logs"

	"actor"
)

//go:generate go -C ../../../../.. run ./cmd/gercmd gen -force -tmpl cmd/noteserver/templates/shard_export.tmpl -recv Hub cmd/noteserver/src/mods/auth/auth_mgr.go cmd/noteserver/src/service

// AuthMgr 管账号。它被挂在按手机号分片的 auth actor 上。
//
// 为什么注册要走 actor：注册是"先查重、再建号"两步，中间有窗口。
// 按手机号哈希分片之后，同一个号码永远落到同一个 actor，两步天然串行，
// 不必依赖数据库的唯一约束——这一点在换成 Norm 之后尤其重要，
// 因为 Norm 的异步 Save 根本没有唯一冲突可以报（见 databases/account.go）。
//
// 一个功能文件夹只能有一个 **_mgr.go / **_mod.go。真要细分，就在本文件夹里
// 加 **_imp.go 定义 **Imp 对象，由 AuthMgr 持有——那个对象不再组合 ModObj，
// 因为跨 goroutine 的入口只该有一个。
type AuthMgr struct {
	actor.ModObj[*AuthMgr]
	accounts contract.IAccountStore
}

// NewAuthMgr 建模块。
//
// 返回 actor.IModule 而不是 *AuthMgr 是规范要求：模块只该通过 AddModule 交给
// loader，返回具体类型就给了调用方"绕过 actor 直接调方法"的机会——那样改的是
// 别人事件循环上的状态，锁全白加了。
//
// Init 靠字段偏移反推宿主指针，此后这个结构体不能再被拷贝——
// 拷贝出来的那份里，ModObj 记的仍然是原对象的地址。
func NewAuthMgr(accounts contract.IAccountStore) actor.IModule {
	m := &AuthMgr{accounts: accounts}
	m.Init()
	return m
}

// Register 查重并建号，返回账号 ID。手机号已存在时返回 contract.ErrPhoneTaken。
//
// passwordHash 由调用方（HTTP 层）预先算好，见包注释。
//
//	export: AuthRegister
func (that *AuthMgr) Register(uid string, passwordHash string) (string, error) {
	info, err := that.accounts.Create(uid, passwordHash)
	if err != nil {
		return "", err
	}
	return info.UID, nil
}

// Lookup 取出账号，供 HTTP 层比对密码。
//
// 返回的是 comm.AccountSnap 而不是本模块自己的类型——规范要求跨模块只传
// **Snap。哈希跟着快照一起离开 actor，只是在同一个进程内传递，HTTP 层比对完
// 就丢；换来的是 bcrypt 那几十毫秒不占用事件循环。
//
//	export: AuthLookup
func (that *AuthMgr) Lookup(uid string) (comm.AccountSnap, error) {
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
func (that *AuthMgr) TouchLogin(uid string, at time.Time) {
	err := that.accounts.TouchLogin(uid, at)
	switch {
	case err == nil:
	case errors.Is(err, contract.ErrAccountNotFound):
		// 账号刚验过密码，这里查不到只可能是并发注销，不值得记一条错误日志
	default:
		logs.Warnf("[AuthMgr] 记录 %s 的登录时间失败: %v", uid, err)
	}
}
