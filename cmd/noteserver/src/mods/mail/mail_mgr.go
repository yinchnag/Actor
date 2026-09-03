// Package mail 是邮件功能，由两个模块组成：
//
//	mail_mgr.go     MailMgr      挂在服务器上，按 UID 分片，管存储
//	mailbox_mod.go  MailboxMod   挂在用户身上，管这个用户在线期间的信箱视图
//
// 为什么要两个
// -----------
//
// MailMgr 必须存在：运维要能给**离线**用户下发，那时他根本没有 actor。
//
// MailboxMod 是为了读。用户量大时一个 mgr 分片服务成千上万个用户，每次拉取
// 邮件都回 mgr 要一遍，等于把所有人的读请求排到那几条事件循环上。让用户自己的
// actor 持有一份信箱视图之后，拉取是本地读，完全不出这条协程。
//
// 谁读谁写
// --------
//
//	写：rut → MailMgr.Send / Claim       权威侧改存储，**带返回值同步给结果**
//	读：rut → MailboxMod.List            本地读，不出用户那条协程
//	同步：MailMgr → MailboxMod.Push      改完之后单向通知，不等回执
//	上线：rut/Hub → MailMgr.Fetch        单向通知，让 mgr 推一次
//
// 两条不同的规矩，别混：
//
//   - **rut → 模块可以带返回值。** rut 跑在 gin 的请求协程上，不会被任何模块
//     调用，阻塞它只影响这一个 HTTP 请求。
//   - **mgr ↔ mod 之间不可以带返回值，没有例外。** 两边都是事件循环，一个模块
//     方法只要在等另一个 actor 的返回值，它自己的队列就停着不动；互相等就是死锁，
//     而框架的环检测只覆盖同步调用，写成同步之后能救你的只有 3 秒超时。
//     觉得需要反参时该换的是设计：要数据就反过来推，要结果就让 rut 直连权威方。
//
// 曾经把写做成"rut → mod → 异步通知 mgr"，是错的：mod 立刻返回了自己手上那份
// 还没改的数据，客户端据此以为领取成功了，而 mgr 那边的读改写还没跑、甚至可能
// 失败。**权威状态在存储上，而存储只有 mgr 碰得到**——"改完了吗、改成什么样"
// 这类问题只有 mgr 答得准，让 mod 去答它只能猜。所以写必须直连权威侧。
package mail

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"noteserver/src/comm"
	"noteserver/src/contract"
	"noteserver/src/logs"

	"actor"
)

//go:generate go -C ../../../../.. run ./cmd/gercmd gen -force -tmpl cmd/noteserver/templates/shard_export.tmpl -recv Hub cmd/noteserver/src/mods/mail/mail_mgr.go cmd/noteserver/src/service

// mailPusher 是 MailMgr 需要的"把信箱推给某个用户"的能力。
//
// 声明在本包而不是直接用 *service.Hub：mods **不能 import service**（会与
// service → mods 成环，分层测试也会拦）。所以模块声明一个只含所需方法的小接口，
// 由 Hub 满足它、在构造时注入——这正是规范第 7 条里写的那个做法。
//
// 方法名 MailboxPush 就是 gercmd 从 MailboxMod.Push 上的 export: 标记生成的门面
// （src/service/mailbox_export.go）。所以这条边仍然走的是导出函数，没有绕过规范。
type mailPusher interface {
	MailboxPush(gid uint64, uid string, mails []comm.MailSnap)
}

// MailMgr 管所有用户的邮件，是这个功能唯一碰存储的地方。
//
// 它**没有缓存**：一个分片服务成千上万个用户，缓存所有人的信箱等于把内存交给
// 运维的下发量决定。真正的读缓存在用户侧的 MailboxMod 手上，一个用户一份。
type MailMgr struct {
	actor.ModObj[*MailMgr]
	mails contract.IMailStore
	push  mailPusher
}

// NewMailMgr 建模块。push 用来把信箱推回给在线用户。
func NewMailMgr(mails contract.IMailStore, push mailPusher) actor.IModule {
	m := &MailMgr{mails: mails, push: push}
	m.Init()
	return m
}

// Send 给某个用户下发一封邮件。运维后台调它。
//
// **不检查用户是否在线，也不检查用户是否存在**：前者正是这个模块做成 Mgr 的原因，
// 后者是运维的责任——在这里查账号会把 mail 分片和 auth 分片耦在一起。
//
// 信箱满时顶掉**最老的一封**而不是拒收：运维下发不该因为用户信箱满了而失败。
//
// 它有返回值，因为调用方是 rut 层（运维后台的 HTTP 请求），不是别的模块。
// 下发完成后顺带把新信箱推给在线的那个用户——那一跳才是模块间的调用，是通知。
//
//	export: MailSend
func (that *MailMgr) Send(uid string, title string, content string, items []comm.MailItemSnap) (comm.MailSnap, error) {
	box, err := that.mails.Load(uid)
	if err != nil {
		return comm.MailSnap{}, err
	}

	id, err := newMailID(uid)
	if err != nil {
		return comm.MailSnap{}, err
	}
	if items == nil {
		items = []comm.MailItemSnap{} // 序列化成 [] 而不是 null，客户端好接
	}
	m := comm.MailSnap{
		MailID:  id,
		Title:   title,
		Content: content,
		Items:   items,
		SendAt:  time.Now().UnixMilli(),
	}

	// 新的排最前。信箱始终按下发时间倒序，于是"最老的一封"永远是末尾那个，
	// 顶掉它就是切一刀——不必每次排序找最小值。
	box = append([]comm.MailSnap{m}, box...)
	if len(box) > comm.MailKeepLimit {
		box = box[:comm.MailKeepLimit]
	}

	if err := that.mails.Save(uid, box); err != nil {
		return comm.MailSnap{}, err
	}
	that.pushTo(uid, box)
	return m, nil
}

// Fetch 把某个用户的信箱推给他的 MailboxMod。用户上线时、以及 rut 发现视图还
// 没就绪时调它。
//
// **无返回值**：这是一次"你去干件事"的通知，调用方（Hub 或 rut）不关心结果，
// 结果会以 Push 的形式落到用户 actor 上。即使调用方眼下都不是模块，也不该写成
// 带返回值的——那样 rut 要等 mgr 读完存储，而它等的东西自己根本用不上；
// 更要紧的是哪天有模块要调它时，签名已经是错的了。
//
//	export: MailFetch
func (that *MailMgr) Fetch(uid string) {
	box, err := that.mails.Load(uid)
	if err != nil {
		// 没有调用方能接这个错。用户那边会一直停在 Ready=false，
		// 客户端重试一次就会再走一遍这里。
		logs.Warnf("[MailMgr] 读取 %s 的信箱失败: %v", uid, err)
		return
	}
	that.pushTo(uid, box)
}

// Claim 领取附件，返回领到的道具。**由 rut 直接调用**。
//
// 它**有返回值**，因为这是一次写操作，调用方需要知道到底成没成：领到了什么、
// 还是这封已经领过了、还是压根不存在。这些问题只有权威侧答得准——存储只有 mgr
// 碰得到。让用户侧的 MailboxMod 转发再立刻返回本地快照，等于拿一份没改过的旧
// 数据冒充操作结果，客户端会以为成功了。
//
// "只能领一次"就落在这里：同一个 UID 的所有邮件操作都落在同一个 mgr 分片上，
// 所以这段"读—判断—写"天然串行，不需要任何锁。
//
// 改完之后把新信箱推给在线的那个用户——那一跳才是模块间调用，是单向通知。
//
//	export: MailClaim
func (that *MailMgr) Claim(uid string, mailID string) ([]comm.MailItemSnap, error) {
	box, err := that.mails.Load(uid)
	if err != nil {
		return nil, err
	}

	idx := -1
	for i := range box {
		if box[i].MailID == mailID {
			idx = i
			break
		}
	}
	switch {
	case idx < 0:
		return nil, comm.ErrMailNotFound
	case box[idx].Claimed:
		return nil, comm.ErrMailAlreadyClaimed
	case len(box[idx].Items) == 0:
		// 没有附件的邮件不该被"领取"——那样会把 Claimed 置真，
		// 让人误以为领到过东西。
		return nil, comm.ErrMailNotFound
	}

	box[idx].Claimed = true
	if err := that.mails.Save(uid, box); err != nil {
		return nil, err
	}

	// 拷一份再返回：box 是从存储读出来的，直接把它内部的切片交出去，
	// 调用方（HTTP 协程）就和这里共享了同一块底层数组。
	items := make([]comm.MailItemSnap, len(box[idx].Items))
	copy(items, box[idx].Items)

	that.pushTo(uid, box)
	return items, nil
}

// pushTo 把信箱推给用户侧的 MailboxMod。
//
// 用户不在线时这一跳会被生成的门面直接丢弃（见 templates/user_notify.tmpl），
// 这里不必判断在线与否——那是 Hub 才知道的事。
//
// gid 用 actor.CurrentGID() 现取：模块方法跑在自己的事件循环上，拿不到 loader，
// 所以享受不到 GetGoroutineID 那条便宜路径。约 1.7µs，而这里都不是热路径
// （下发、上线、领取），可以接受。
func (that *MailMgr) pushTo(uid string, box []comm.MailSnap) {
	if box == nil {
		box = []comm.MailSnap{}
	}
	that.push.MailboxPush(actor.CurrentGID(), uid, box)
}

// newMailID 生成邮件 ID：uid-毫秒时间戳-4字节随机。
//
// 随机后缀不能省。运维批量下发时同一毫秒可能发出多封，光靠时间戳会撞——
// 而撞了之后领取会领到错的那封。
func newMailID(uid string) (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成邮件ID失败: %w", err)
	}
	return fmt.Sprintf("%s-%013d-%s", uid, time.Now().UnixMilli(), hex.EncodeToString(buf[:])), nil
}
