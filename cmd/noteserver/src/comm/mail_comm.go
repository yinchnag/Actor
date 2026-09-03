package comm

import "errors"

// MailBoxSnap 是用户信箱的一次视图快照，由用户侧的 MailboxMod 交出来。
//
// 它和 []MailSnap 的区别只有一个 Ready：数据到了没有。
//
// 用户侧的信箱是登录后由 MailMgr **推**过来的，推到之前 mod 手上是空的。
// 返回一个 Ready=false 的空信箱，比返回一个看起来像"你没有邮件"的空列表诚实——
// 这两种状态在客户端要显示成不同的东西（转圈 vs 空信箱），服务端把它们混成
// 一个 200 + 空列表，客户端就再也分不开了。
//
// Ready 不进存储：它描述的是"这个用户当前这条 actor 上的情况"。
type MailBoxSnap struct {
	Ready bool       `json:"ready"`
	Mails []MailSnap `json:"mails"`
}

// 邮件的业务错误。
//
// 放在 comm 而不是 mods/mail：路由要把它们翻译成 HTTP 状态码，而 router 引
// mods 会开出一条新的依赖边——路由从此能碰到模块包，规范第 7 条"必须走导出
// 函数"就少了一道结构上的保障。放在 comm 里两边都能引，依赖图不变。
//
// 也不放 contract：那里只放**接口**与接口返回的错误，而这两个是模块的业务判断，
// 存储层根本不知道有这回事。
var (
	// ErrMailNotFound 邮件不存在、已被顶掉，或者那封邮件根本没有附件。
	ErrMailNotFound = errors.New("邮件不存在")
	// ErrMailAlreadyClaimed 附件已经领过了。
	ErrMailAlreadyClaimed = errors.New("附件已领取")
	// ErrMailBoxNotReady 信箱数据还没从 MailMgr 推过来。
	ErrMailBoxNotReady = errors.New("信箱数据尚未就绪")
)

// MailItemSnap 是邮件携带的一件道具。
//
// 只有物品 ID 和数量——道具的名字、图标、堆叠上限这些都属于配表，不该抄进邮件里：
// 抄了就意味着改配表要回头改历史邮件。发的时候记 ID，领的时候按当时的配表解释。
type MailItemSnap struct {
	ItemID int `json:"item_id"`
	Count  int `json:"count"`
}

// MailSnap 是一封邮件的跨模块快照，同时也是直接序列化给客户端的形状。
type MailSnap struct {
	MailID  string `json:"mail_id"`
	Title   string `json:"title"`
	Content string `json:"content"`
	// Items 是附件。没有附件时是空切片而不是 nil——序列化成 [] 比 null 好接。
	Items []MailItemSnap `json:"items"`
	// SendAt 下发时间（毫秒时间戳）。排序与"顶掉最老一封"都按它。
	SendAt int64 `json:"send_at"`
	// Claimed 附件是否已领取。没有附件的邮件这个字段恒为 false，客户端不必特殊处理。
	Claimed bool `json:"claimed"`
}

// 邮件功能的常量。只有 mail 这一个功能在用。
const (
	// MailShards 是 mail actor 的分片数。
	//
	// 邮件必须按 UID 分片，理由和注册一样是"读改写"：新增一封要先读出整个信箱、
	// 追加、超了 MailKeepLimit 再顶掉最老的、写回。同一个 UID 落到同一个 actor，
	// 这三步天然串行；不然两封同时下发就可能互相覆盖。
	MailShards = 4

	// MailKeepLimit 每个用户最多保留的邮件数。
	//
	// 到达上限后新邮件顶掉**最老的一封**，不是拒收——运维下发不该因为用户信箱
	// 满了而失败。代价是最老那封连同它没领的附件一起消失，所以这个数不能太小。
	MailKeepLimit = 100

	// MaxMailTitleRunes / MaxMailContentRunes 标题与正文的字数上限。
	//
	// 一个信箱最多 MailKeepLimit 封，整个信箱是**一条存储记录**（见
	// databases/mail.go），所以上限乘以 100 就是单条记录的量级：
	// 100 × (64 + 2000) 字符，最坏全是 4 字节字符也才 ~800KB，
	// 在 Norm 的 JSON 列（MySQL JSON 上限 1GB）里绰绰有余。
	MaxMailTitleRunes   = 64
	MaxMailContentRunes = 2000

	// MaxMailItems 单封邮件最多携带的道具种类数。
	MaxMailItems = 20
)
