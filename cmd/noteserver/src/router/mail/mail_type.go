package mail

import (
	"noteserver/src/comm"

	"web"
)

// SendRequest 运维后台下发邮件。→ POST /api/mail/send
//
// 它挂在 /api/mail 分组下、由运维令牌中间件保护，而不是用户的 Bearer——
// 下发的对象是 UID，操作者是运维，两者是不同的身份。
type SendRequest struct {
	web.POST `path:"/mail/send"`
	UID      string              `json:"uid"`
	Title    string              `json:"title"`
	Content  string              `json:"content"`
	Items    []comm.MailItemSnap `json:"items"`
}

// SendResponse 下发结果。
type SendResponse struct {
	Mail comm.MailSnap `json:"mail"`
}

// ListRequest 用户拉取自己的邮件。→ GET /api/mails
//
// 没有 uid 字段：拉谁的邮件由 Bearer 令牌决定，不由请求体决定——
// 让客户端传 uid 等于把"读别人的邮件"做成了一个参数。
type ListRequest struct {
	web.GET `path:"/mails"`
}

// ListResponse 邮件列表。
//
// count 是本次返回的条数。信箱上限是 comm.MailKeepLimit，所以它同时就是总数。
//
// ready=false 时 mails 一定是空的——那表示数据还在从 MailMgr 推过来的路上，
// 不是"你没有邮件"。这两种情况在客户端要显示成不同的东西，所以必须分得开。
type ListResponse struct {
	Count int             `json:"count"`
	Ready bool            `json:"ready"`
	Mails []comm.MailSnap `json:"mails"`
}

// ClaimRequest 领取附件。→ POST /api/mails/claim
type ClaimRequest struct {
	web.POST `path:"/mails/claim"`
	MailID   string `json:"mail_id"`
}

// ClaimResponse 领到的道具。
//
// 它是**权威结果**：领取由 rut 直连 MailMgr 完成，返回时存储已经改好了。
// 早先这里返回的是"已受理"加一份用户侧的旧快照，那是错的——rut 拿到的不是
// 操作结果，客户端会以为成功了，而权威侧可能根本没改成（已领过、已被顶掉）。
//
// 真实游戏里道具不该由这个接口回给客户端——应当由服务端调背包模块入包，
// 客户端只收到"领取成功"。那一跳是模块间调用，必须是单向通知。
type ClaimResponse struct {
	Items []comm.MailItemSnap `json:"items"`
}
