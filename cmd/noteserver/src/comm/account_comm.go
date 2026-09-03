package comm

import "time"

// AccountSnap 是账号的跨模块快照。
//
// 名字里的 Snap 是规范要求：本包里的 struct 一律以 Snap 结尾，用来标记
// "这是可以跨模块传递的值"。反过来说，不带 Snap 的类型就不该出现在
// 模块与模块之间——那类型属于某个模块自己，别人不该认识它。
//
// 它是**值快照**，不是存储对象。databases.Account 内嵌了 Norm 的 TableSchema，
// 带着 unsafe 指针和一次性初始化状态，一旦离开 databases 包被随手拷贝，
// selfPtr 就会指向旧对象。让那个类型止步于 databases，跨层只传这里的值。
type AccountSnap struct {
	UID          string // 账号唯一 ID，本项目里就是手机号
	PasswordHash string // bcrypt 哈希
	RegisterDate int64  // 注册时间（毫秒时间戳）
	LoginDate    int64  // 最近登录时间（毫秒时间戳）
}

// 账号功能的常量。只有 auth 这一个功能在用，所以放这里而不是 consts_comm.go。
const (
	// SessionTTL 登录会话有效期。
	//
	// 注意它必须 <= data/orm.json 里的 redis.key_ttl_sec：会话是用 Norm 的
	// SaveR/LoadR 存进 Redis 的，而 Norm 的 TTL 是连接池级别的全局设置，
	// 没有按对象设置 TTL 的入口。所以这里写 7 天，配置里也必须是 604800。
	SessionTTL = 7 * 24 * time.Hour

	// MinPasswordLen 密码最少位数。
	MinPasswordLen = 8
	// MaxPasswordBytes 密码最多字节数。
	//
	// 72 是 bcrypt 的硬限制：超过 72 字节的部分会被它直接忽略。
	// 不显式拒掉的话，用户以为自己设了 100 位密码，实际只有前 72 字节生效。
	MaxPasswordBytes = 72

	// AuthShards 是 auth actor 的分片数。
	//
	// 分片的目的不是提高吞吐，而是让"同一个手机号的注册请求落到同一个 actor"
	// 从而天然串行；分成多片则是为了别让所有注册/登录挤在一条事件循环上。
	AuthShards = 4
)
