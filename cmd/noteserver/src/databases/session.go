package databases

import (
	"time"

	"noteserver/src/contract"

	"github.com/norm/orm"
)

// Session 登录会话。
//
// 它是本包里唯一一张**只走 Redis** 的表：读写用 SaveR/LoadR，不进 MySQL
// 存档队列。会话是纯临时态，重启后从 Redis 恢复即可，落 MySQL 只会给
// 刷盘 worker 添堵，还得额外操心过期行的清理。
//
// 代价说清楚：Init() 里的 AutoMigrate 照样会在 MySQL 建一张 session 表，
// Norm 没有"只要 Redis 不要表"的开关。那张表会一直是空的，可以无视。
type Session struct {
	orm.TableSchema[*Session]
	Token string `orm:"primary,name:token,comment:会话令牌,length:64,notNull"`
	UID   string `orm:"name:uid,comment:所属账号,length:32,notNull"`
}

// SessionStore 是 contract.ISessionStore 的 Norm 实现。
type SessionStore struct{}

// NewSessionStore 建会话存储。
func NewSessionStore() *SessionStore {
	(&Session{}).Init()
	return &SessionStore{}
}

// Put 写会话。
//
// ttl 参数被忽略——Norm 的 Redis TTL 是连接池级别的全局设置（key_ttl_sec），
// 没有按对象设置的入口。所以有效期实际由 data/orm.json 决定，
// comm.SessionTTL 只是把这个约定写在代码里让人看得见。两处必须对齐。
func (that *SessionStore) Put(token, uid string, _ time.Duration) error {
	s := &Session{Token: token, UID: uid}
	s.Init()
	s.SaveR() // 只写 Redis
	return nil
}

// Get 按 token 取 UID。
//
// 用 LoadR 而不是 Load：会话根本没往 MySQL 写过，降级去查只会白跑一趟，
// 而且查到的一定是"没有"——那还不如一开始就只问 Redis，快且语义诚实。
func (that *SessionStore) Get(token string) (string, error) {
	s := &Session{Token: token}
	s.Init()
	if err := s.LoadR(); err != nil {
		if orm.IsNotFound(err) {
			return "", contract.ErrNoSession
		}
		return "", err
	}
	return s.UID, nil
}

// Delete 主动登出。
//
// Norm 的 Delete 是"删 Redis + 异步软删 MySQL"，后半段在这张表上是空转
// （UPDATE 命中 0 行）。为这点空转再写一套只删 Redis 的路径不值得，
// 但得知道它在那儿。
func (that *SessionStore) Delete(token string) error {
	s := &Session{Token: token}
	s.Init()
	s.Delete()
	return nil
}
