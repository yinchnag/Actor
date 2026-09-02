// Package comm 放跨层共享的常量与小工具。
//
// 这里的东西有一个共同点：它们是**程序行为**而不是部署配置。
// 部署配置（监听地址、数据库地址）在 data/ 下的 json 里，改了要重启；
// 这里的常量改了要重新编译——这个区别本身就是一道防线，
// 免得有人把"笔记最多多少字"这种业务规则塞进运维的配置文件里。
package comm

import "time"

const (
	// SessionTTL 登录会话有效期。
	//
	// 注意它必须 <= data/orm.json 里的 redis.key_ttl_sec：会话是用 Norm 的
	// SaveR/LoadR 存进 Redis 的，而 Norm 的 TTL 是连接池级别的全局设置，
	// 没有按对象设置 TTL 的入口。所以这里写 7 天，配置里也必须是 604800。
	SessionTTL = 7 * 24 * time.Hour

	// MaxNoteRunes 单条笔记的字数上限。
	//
	// 需求是"至少能存 800 汉字"。这里给到 10000，理由是 Norm 把没有 length
	// 标记的 string 字段映射成 MySQL 的 TEXT（65535 字节）——10000 个字符
	// 即使全是 4 字节的 emoji 也只有 40000 字节，离上限还有一半余量。
	// 想再往上加就必须先解决列类型：TEXT 装不下 20000 个 4 字节字符。
	MaxNoteRunes = 10000

	// MinPasswordLen 密码最少位数。
	MinPasswordLen = 8
	// MaxPasswordBytes 密码最多字节数。
	//
	// 72 是 bcrypt 的硬限制：超过 72 字节的部分会被它直接忽略。
	// 不显式拒掉的话，用户以为自己设了 100 位密码，实际只有前 72 字节生效。
	MaxPasswordBytes = 72

	// MaxBodyBytes 请求体上限，防止有人拿超大 body 打内存。
	//
	// 256KB 这个数不是随手取的整数，它对齐的是 net/http 的 maxPostHandlerReadBytes：
	// handler 提前拒掉请求、请求体还没读完时，服务器会替你把剩下的读掉丢弃
	// （"drain"），好让这条 keep-alive 连接停在一个干净的消息边界上、能继续复用；
	// 但 drain 是被害者出钱，所以有上限——剩余超过 256KB 就直接断连接。
	//
	// 把上限压到正好等于那个额度，中间那段"能收下但拒了就得断连"的灰区就没了：
	//
	//	≤256KB  收下。就算解析失败也只丢一个请求，连接照常复用。
	//	>256KB  MaxBytesReader 当场掐断，服务器不会为一个注定被拒的请求
	//	        一路缓冲到 1MB。
	//
	// 够不够用：一条笔记最多 MaxNoteRunes(10000) 个字符，客户端就算把每个字符
	// 都写成 \uXXXX 转义（非 BMP 是代理对，12 字节/字符）也才 ~120KB，余量翻倍。
	MaxBodyBytes = 256 << 10

	// NoteListLimit 一次返回的最大笔记条数，也是 NoteMod 缓存持有的条数。
	NoteListLimit = 200
)

const (
	// AuthShards 是 auth actor 的分片数。
	//
	// 分片的目的不是提高吞吐，而是让"同一个手机号的注册请求落到同一个 actor"
	// 从而天然串行；分成多片则是为了别让所有注册/登录挤在一条事件循环上。
	AuthShards = 4

	// UserIdleTimeout 用户 actor 空闲多久后回收。
	// 在线用户数决定 goroutine 数，不回收的话每个登录过的用户都会留一条协程。
	UserIdleTimeout = 5 * time.Minute

	// JanitorInterval 空闲回收的巡检间隔。
	JanitorInterval = 30 * time.Second
)
