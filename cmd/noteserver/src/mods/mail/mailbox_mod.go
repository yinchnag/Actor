package mail

import (
	"sort"

	"noteserver/src/comm"

	"actor"
)

//go:generate go -C ../../../../.. run ./cmd/gercmd gen -force -tmpl cmd/noteserver/templates/user_export.tmpl -recv Hub cmd/noteserver/src/mods/mail/mailbox_mod.go cmd/noteserver/src/service

// MailboxMod 是一个用户在线期间的信箱**只读视图**。
//
// 职责划分：**mod 只负责读，mgr 负责写、兼顾读**。
//
//	读：rut → MailboxMod.List          本地读，不出这条协程
//	写：rut → MailMgr.Send/Claim       权威侧改存储，同步返回结果
//	同步：MailMgr → MailboxMod.Push    改完之后单向通知，不等回执
//
// 为什么写不能走 mod 转发（这里踩过一次）
// -------------------------------------
//
// 早先的写法是 rut 调 mod、mod 本地标记"处理中"再异步通知 mgr、然后**立刻返回
// 本地那份还没改的数据**。那是错的，而且错得很隐蔽：rut 拿到的返回值根本不是
// 操作结果，只是一份旧快照。客户端据此以为领取成功了，可 mgr 那边的读改写
// 还没跑，甚至可能失败（邮件已被顶掉、附件已领过）。
//
// 根子在于：**权威状态在存储上，而存储只有 mgr 碰得到**。任何"改完了吗、
// 改成什么样了"的问题，只有 mgr 答得准。让 mod 去答，它只能猜。
//
// 所以写一律 rut 直连 mgr，并且**带返回值**——这条边是允许阻塞的：rut 跑在
// gin 的请求协程上，不会被任何模块调用，阻塞它只影响这一个 HTTP 请求。
// 真正不能带返回值的是 mgr ↔ mod 那条边，两边都是事件循环，互相等就是死锁。
//
// 结果是 MailboxMod **一条对外调用都没有**：它只接收推送、只回答读请求，
// 永远不会停下来等任何人。这个性质比"小心别写成同步"可靠得多。
type MailboxMod struct {
	actor.ModObj[*MailboxMod]

	uid   string
	ready bool // MailMgr 推过数据没有
	// mails 按下发时间倒序，与 MailMgr 那边保持同一口径。
	mails []comm.MailSnap
}

// NewMailboxMod 建模块。uid 决定这个 actor 服务哪个账号。
//
// 没有任何依赖注入——它不调别人，自然也不需要别人的能力。
func NewMailboxMod(uid string) actor.IModule {
	m := &MailboxMod{uid: uid}
	m.Init()
	return m
}

// Push 接收 MailMgr 推过来的完整信箱。
//
// **无返回值**，这是 mgr → mod 唯一的一条边，也是这个模块唯一的写入口。
// 整份替换而不是增量合并：增量要处理乱序、丢包、去重，而信箱最多
// comm.MailKeepLimit 封，整份推的代价完全可以接受。用简单换正确。
//
//	export: MailboxPush
func (that *MailboxMod) Push(mails []comm.MailSnap) {
	sort.SliceStable(mails, func(i, j int) bool { return mails[i].SendAt > mails[j].SendAt })
	that.mails = mails
	that.ready = true
}

// List 交出当前的信箱视图。
//
// 它**有返回值**，而且这是合规的：调用方是 rut，跑在 gin 的请求协程上，
// 不会被任何模块调用。这条界线就是 mgr↔mod"只通知"与 rut→模块"可以要返回值"
// 的分界。
//
// 数据还没到时不假装成"你没有邮件"：返回 Ready=false，由 rut 去催一次 mgr
// 并回 202。这两种状态在客户端要显示成不同的东西，混成一个就再也分不开。
//
// 注意它**不会**为了"催一下"而调 mgr——那会让这个模块重新长出一条对外调用。
// 催谁、什么时候催是 rut 的事。
//
//	export: MailboxList
func (that *MailboxMod) List() (comm.MailBoxSnap, error) {
	// 拷一份再返回：mails 归这条事件循环独占，直接把切片交出去，
	// 调用方（HTTP 协程）就和事件循环共享了同一块底层数组，
	// 下一次 Push 就是数据竞争。
	out := make([]comm.MailSnap, len(that.mails))
	copy(out, that.mails)
	return comm.MailBoxSnap{Ready: that.ready, Mails: out}, nil
}
