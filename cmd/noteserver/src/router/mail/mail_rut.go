// Package mail 是邮件的三个接口：运维下发、用户拉取、领取附件。
package mail

import (
	"errors"
	"net/http"
	"strconv"
	"unicode/utf8"

	"noteserver/src/bases"
	"noteserver/src/comm"
	"noteserver/src/middleware"
	"noteserver/src/security"

	"github.com/gin-gonic/gin"

	"actor"
	"web"
)

// MailRut 持有路由需要的依赖。
type MailRut struct {
	web.Router[*MailRut]
	hub service
}

// service 是 MailRut 用到的 Hub 能力，三个都是 gercmd 生成的门面
// （src/service/mail_export.go）。
// 注意读写走的是**两个不同的模块**：
//
//	写 MailSend    → MailMgr     运维下发，权威侧改存储
//	写 MailClaim   → MailMgr     领取附件，权威侧改存储，**同步给结果**
//	读 MailboxList → MailboxMod  用户侧的视图，本地读，不出他那条协程
//	催 MailFetch   → MailMgr     视图还没就绪时让 mgr 推一次（无返回值）
//
// **写一律直连 mgr，不经过 mod 转发。** 转发过一次，错得很隐蔽：mod 立刻返回了
// 自己手上那份还没改的数据，rut 拿到的根本不是操作结果，客户端会以为成功了。
// 权威状态在存储上，而存储只有 mgr 碰得到。
//
// 前三个有返回值是合规的：rut 跑在 gin 的请求协程上，不会被任何模块调用，
// 阻塞它只影响这一个 HTTP 请求。必须无返回值的是 mgr ↔ mod 那条边。
type service interface {
	MailSend(gid uint64, uid string, title string, content string, items []comm.MailItemSnap) (comm.MailSnap, error)
	MailClaim(gid uint64, uid string, mailID string) ([]comm.MailItemSnap, error)
	MailboxList(gid uint64, uid string) (comm.MailBoxSnap, error)
	MailFetch(gid uint64, uid string)
}

// NewOps 建运维侧的路由（下发邮件），挂到已套好运维令牌中间件的分组上。
//
// 邮件的三个接口分属**两种身份**：下发归运维，拉取与领取归用户。这一层被
// web.Router 的形状逼着拆成两个对象——Init 会把宿主的**所有**公有方法都扫成
// 路由，没法让同一个对象的一半挂在运维分组、另一半挂在用户分组。
//
// 拆开反而更诚实：它们本来就是两种身份，鉴权也不同。谁套哪个中间件是装配期的
// 部署决定，由 main 决定，不藏在请求类型里。
func NewOps(group gin.IRoutes, hub service) *MailRut {
	m := &MailRut{hub: hub}
	m.Init(group, bases.RouterOpts()...)
	return m
}

// NewUser 建用户侧的路由（拉取、领取），挂到已套好 Bearer 鉴权的分组上。
func NewUser(group gin.IRoutes, hub service) *MailUserRut {
	u := &MailUserRut{hub: hub}
	u.Init(group, bases.RouterOpts()...)
	return u
}

// Send 运维后台下发邮件。→ POST /api/mail/send
//
// 不校验收件人是否存在或是否在线：那正是邮件做成 Mgr 的原因（见 mods/mail）。
func (that *MailRut) Send(req *SendRequest, ctx *gin.Context) {
	if !security.ValidPhone(req.UID) {
		bases.Fail(ctx, http.StatusBadRequest, "收件人 UID 格式不正确")
		return
	}
	if msg, ok := checkMailText(req.Title, req.Content); !ok {
		bases.Fail(ctx, http.StatusBadRequest, msg)
		return
	}
	if msg, ok := checkItems(req.Items); !ok {
		bases.Fail(ctx, http.StatusBadRequest, msg)
		return
	}

	m, err := that.hub.MailSend(actor.CurrentGID(), req.UID, req.Title, req.Content, req.Items)
	if err != nil {
		bases.FailInvoke(ctx, err, "下发邮件")
		return
	}
	bases.JSON(ctx, http.StatusCreated, SendResponse{Mail: m})
}

// MailUserRut 是面向用户的那两个接口。
//
// 单独一个类型是被 Router 的形状逼出来的：Init 会把宿主的所有公有方法都扫成
// 路由，没法让同一个对象的一半方法挂在运维分组、另一半挂在用户分组。
// 拆成两个对象反而更诚实——它们本来就是两种身份。
type MailUserRut struct {
	web.Router[*MailUserRut]
	hub service
}

// List 用户拉取自己的邮件。→ GET /api/mails
//
// 读的是用户自己 actor 上的信箱视图，不出他这条协程——用户量大时这一点很要紧：
// 回 MailMgr 要的话，所有人的读请求都会排到那几条分片上。
//
// ready=false 表示数据还在路上（登录后由 MailMgr 推过来）。这时返回 200 + 一个
// 空列表会让客户端以为"我没有邮件"，所以改回 202：请求收到了，结果还没到。
func (that *MailUserRut) List(_ *ListRequest, ctx *gin.Context) {
	uid := middleware.UID(ctx)

	gid := actor.CurrentGID()
	box, err := that.hub.MailboxList(gid, uid)
	if err != nil {
		bases.FailInvoke(ctx, err, "获取邮件")
		return
	}
	if !box.Ready {
		// 视图还没就绪，催 mgr 推一次（无返回值，不等它），回 202。
		// 催谁、什么时候催是 rut 的事——放在 mod 里会让那个模块重新长出
		// 一条对外调用，而它现在一条都没有。
		that.hub.MailFetch(gid, uid)
		bases.JSON(ctx, http.StatusAccepted, ListResponse{Ready: false, Mails: []comm.MailSnap{}})
		return
	}
	bases.JSON(ctx, http.StatusOK, ListResponse{Count: len(box.Mails), Ready: true, Mails: box.Mails})
}

// Claim 领取附件。→ POST /api/mails/claim
func (that *MailUserRut) Claim(req *ClaimRequest, ctx *gin.Context) {
	uid := middleware.UID(ctx)
	if req.MailID == "" {
		bases.Fail(ctx, http.StatusBadRequest, "缺少 mail_id")
		return
	}

	// 直连 MailMgr：写操作必须由权威侧给结果。
	items, err := that.hub.MailClaim(actor.CurrentGID(), uid, req.MailID)
	if err != nil {
		// 这两个是业务错误而不是调用失败，各自有各自的语义
		switch {
		case errors.Is(err, comm.ErrMailNotFound):
			bases.Fail(ctx, http.StatusNotFound, "邮件不存在或没有附件")
			return
		case errors.Is(err, comm.ErrMailAlreadyClaimed):
			// 409 而不是 400：请求本身没问题，是资源当前的状态不允许
			bases.Fail(ctx, http.StatusConflict, "附件已经领取过了")
			return
		}
		bases.FailInvoke(ctx, err, "领取附件")
		return
	}
	// 200：领取已经落定了，items 就是权威结果。
	//
	// 用户侧那份视图由 mgr 异步推新，可能比这个响应晚一点点到——客户端紧接着
	// 再拉一次列表时，理论上有极小的窗口仍看到 claimed=false。真实游戏里这一步
	// 是服务端推送，客户端不会靠轮询；这里如实说明而不是假装没有。
	bases.JSON(ctx, http.StatusOK, ClaimResponse{Items: items})
}

// checkMailText 校验标题与正文。
func checkMailText(title, content string) (string, bool) {
	if title == "" {
		return "邮件标题不能为空", false
	}
	if n := utf8.RuneCountInString(title); n > comm.MaxMailTitleRunes {
		return "标题最多 " + strconv.Itoa(comm.MaxMailTitleRunes) + " 字，当前 " + strconv.Itoa(n) + " 字", false
	}
	if n := utf8.RuneCountInString(content); n > comm.MaxMailContentRunes {
		return "正文最多 " + strconv.Itoa(comm.MaxMailContentRunes) + " 字，当前 " + strconv.Itoa(n) + " 字", false
	}
	if !utf8.ValidString(title) || !utf8.ValidString(content) {
		return "邮件内容不是合法的 UTF-8", false
	}
	return "", true
}

// checkItems 校验附件。
//
// 数量必须为正：0 个的道具是运维填错了，而负数会在入包时变成扣物品——
// 那是一条把发奖接口变成扣道具接口的路，必须在最外层挡掉。
func checkItems(items []comm.MailItemSnap) (string, bool) {
	if len(items) > comm.MaxMailItems {
		return "单封邮件最多携带 " + strconv.Itoa(comm.MaxMailItems) + " 种道具", false
	}
	for _, it := range items {
		if it.ItemID <= 0 {
			return "道具 ID 必须为正", false
		}
		if it.Count <= 0 {
			return "道具数量必须为正", false
		}
	}
	return "", true
}
