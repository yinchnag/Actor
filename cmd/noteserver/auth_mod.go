package main

import (
	"context"
	"errors"
	"time"

	"actor"
)

// dbTimeout 是模块方法里每次数据库操作的上限。
//
// 必须明显小于框架写死的任务超时（defaultTaskTimeout = 3s）：模块方法跑在 actor
// 的事件循环上，一旦超过 3s，调用方那边已经超时走人了，而 actor 还在这儿干等，
// 队列里后面的请求全被堵住。留 1s 余量给排队和反射开销。
const dbTimeout = 2 * time.Second

// AuthMod 管账号。它被挂在按手机号分片的 auth actor 上。
//
// 为什么注册要走 actor：注册是"先查重、再插入"两步，中间有窗口。
// 按手机号哈希分片之后，同一个号码永远落到同一个 actor，两步天然串行，
// 不依赖数据库唯一索引也不会有两个请求同时通过查重。
// （唯一索引仍然保留，那是多实例部署时的最后一道防线，见 store.go。）
//
// 为什么密码哈希不在这里算：bcrypt 是 50~70ms 的纯 CPU 活，
// 放进事件循环会把整个分片钉死那么久，几十个并发登录就打满了。
// 所以 HTTP 层负责算哈希/验哈希，这里只碰数据库。
type AuthMod struct {
	actor.ModObj[*AuthMod]
	store Store
}

func NewAuthMod(store Store) *AuthMod {
	m := &AuthMod{store: store}
	m.Init() // Init 靠字段偏移反推宿主指针，此后这个结构体不能再被拷贝
	return m
}

type RegisterArgs struct {
	Phone string
	// PasswordHash 由调用方（HTTP 层）预先算好，见上面的说明。
	PasswordHash string
}

type RegisterResult struct {
	UserID int64
}

// Register 查重并建号。手机号已存在时返回 ErrPhoneTaken。
func (m *AuthMod) Register(a RegisterArgs) (RegisterResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	switch _, err := m.store.FindUserByPhone(ctx, a.Phone); {
	case err == nil:
		return RegisterResult{}, ErrPhoneTaken
	case errors.Is(err, ErrUserNotFound):
		// 正常路径，继续建号
	default:
		return RegisterResult{}, err
	}

	id, err := m.store.CreateUser(ctx, a.Phone, a.PasswordHash)
	if err != nil {
		return RegisterResult{}, err
	}
	return RegisterResult{UserID: id}, nil
}

type LookupArgs struct {
	Phone string
}

type LookupResult struct {
	UserID       int64
	PasswordHash string
}

// Lookup 按手机号取出账号与密码哈希，供 HTTP 层做比对。
//
// 哈希离开 actor 只是在同一个进程内传递，HTTP 层比对完就丢；
// 换来的是 bcrypt 那几十毫秒不占用事件循环。
func (m *AuthMod) Lookup(a LookupArgs) (LookupResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	u, err := m.store.FindUserByPhone(ctx, a.Phone)
	if err != nil {
		return LookupResult{}, err
	}
	return LookupResult{UserID: u.ID, PasswordHash: u.PasswordHash}, nil
}
