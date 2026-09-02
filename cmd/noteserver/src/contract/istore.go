package contract

import "time"

// AccountInfo 是账号的只读快照。
//
// 它不是 databases.Account 本身：那个类型内嵌了 Norm 的 TableSchema，
// 带着 unsafe 指针和一次性初始化状态，一旦离开 databases 包被随手拷贝，
// selfPtr 就会指向旧对象。让它止步于 databases 包，跨层只传值快照。
type AccountInfo struct {
	UID          string // 账号唯一 ID，本项目里就是手机号
	PasswordHash string // bcrypt 哈希
	RegisterDate int64  // 注册时间（毫秒时间戳）
	LoginDate    int64  // 最近登录时间（毫秒时间戳）
}

// NoteInfo 是一条笔记的只读快照，同时也是直接序列化给客户端的形状。
type NoteInfo struct {
	NoteID    string `json:"note_id"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"` // 毫秒时间戳
}

// IAccountStore 账号存储。
//
// 没有 context 参数是刻意的：Norm 的 Load/Save 自己用 context.Background，
// 不接受外部 ctx。这里跟着不带，免得给出一个"传了也没用"的假承诺。
// 超时只能靠 data/orm.json 里 DSN 的 timeout/readTimeout/writeTimeout 控制，
// 详见 README 里"数据库超时"一节。
type IAccountStore interface {
	// Find 按 UID 取账号，不存在时返回 ErrAccountNotFound。
	Find(uid string) (AccountInfo, error)
	// Create 建号。UID 已存在时返回 ErrPhoneTaken。
	Create(uid, passwordHash string) (AccountInfo, error)
	// TouchLogin 记一次登录时间。失败不影响登录本身，调用方只记日志。
	TouchLogin(uid string, at time.Time) error
}

// INoteStore 笔记存储。
type INoteStore interface {
	// Insert 存一条笔记，返回落库后的完整快照。
	Insert(uid, content string, createdAt time.Time) (NoteInfo, error)
	// List 取某账号最近 limit 条笔记，按上传时间倒序。
	List(uid string, limit int) ([]NoteInfo, error)
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
