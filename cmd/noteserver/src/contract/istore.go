package contract

import (
	"time"

	"noteserver/src/comm"
)

// IAccountStore 账号存储。
//
// 没有 context 参数是刻意的：Norm 的 Load/Save 自己用 context.Background，
// 不接受外部 ctx。这里跟着不带，免得给出一个"传了也没用"的假承诺。
// 超时只能靠 data/orm.json 里 DSN 的 timeout/readTimeout/writeTimeout 控制，
// 详见 README 里"数据库超时"一节。
type IAccountStore interface {
	// Find 按 UID 取账号，不存在时返回 ErrAccountNotFound。
	Find(uid string) (comm.AccountSnap, error)
	// Create 建号。UID 已存在时返回 ErrPhoneTaken。
	Create(uid, passwordHash string) (comm.AccountSnap, error)
	// TouchLogin 记一次登录时间。失败不影响登录本身，调用方只记日志。
	TouchLogin(uid string, at time.Time) error
}

// INoteStore 笔记存储。
type INoteStore interface {
	// Insert 存一条笔记，返回落库后的完整快照。
	Insert(uid, content string, createdAt time.Time) (comm.NoteSnap, error)
	// List 取某账号最近 limit 条笔记，按上传时间倒序。
	List(uid string, limit int) ([]comm.NoteSnap, error)
}

// IMailStore 邮件存储。
//
// 接口是**按信箱**而不是按单封邮件设计的：整个信箱是一条记录，
// "新增一封、超了顶掉最老的"因此是一次读改写而不是若干条增删。
// 见 databases/mail.go 里为什么这么存。
type IMailStore interface {
	// Load 取某个用户的信箱，按下发时间倒序。信箱不存在时返回空切片而不是错误——
	// 从没收过邮件是正常状态，不该让调用方去分辨 ErrNotFound。
	Load(uid string) ([]comm.MailSnap, error)
	// Save 整体写回信箱。调用方负责已经排好序、也已经裁到上限之内。
	Save(uid string, mails []comm.MailSnap) error
}

// ISessionStore 登录会话存储。
//
// 会话只进 Redis 不落 MySQL：它是纯临时态，重启后从 Redis 恢复即可，
// 写进 MySQL 只会给存档队列添堵。
type ISessionStore interface {
	// Put 写入会话。ttl 参数保留给实现，Norm 实现受全局 key_ttl_sec 约束，
	// 见 comm.SessionTTL 的注释。
	Put(token, uid string, ttl time.Duration) error
	// Get 按 token 取 UID，不存在或已过期时返回 ErrNoSession。
	Get(token string) (string, error)
	// Delete 主动登出用。
	Delete(token string) error
}
