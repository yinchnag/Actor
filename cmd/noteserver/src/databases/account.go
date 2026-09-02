// Package databases 是 Norm ORM 的落地层：表结构定义 + contract 接口的实现。
//
// 三条本包内必须守住的规矩：
//
//  1. orm.TableSchema 必须是结构体的**第一个字段**。Norm 用 unsafe.Pointer(that)
//     直接当宿主基址，偏移不为 0 时所有字段读写都会落到错误的内存上。
//  2. 带 TableSchema 的对象不能拷贝。Init() 把 selfPtr 绑到了那一份内存上，
//     拷贝之后新对象的 selfPtr 仍指向旧对象——所以跨包一律传 contract 里的值快照。
//  3. Init() 会触发 AutoMigrate 并在失败时 panic，因此它必须在 orm.InitPool
//     成功之后才被调用。本包所有 Init 都推迟到 store 构造函数里。
package databases

import (
	"errors"
	"time"

	"noteserver/src/comm"
	"noteserver/src/contract"

	"github.com/norm/orm"
)

// Account 账号表。
//
// 主键直接就是手机号，不再像原来那样用自增 id：Norm 的 Save 是异步落 MySQL 的，
// 拿不回 LastInsertId，自增主键在这套 ORM 下没有可用的回填路径。
// 好在手机号本身就是天然主键，用它当 pk 还顺带省掉了一次"按手机号查 id"。
type Account struct {
	orm.TableSchema[*Account]
	UID          string `orm:"primary,name:uid,comment:账号唯一ID（手机号）,length:32,notNull"`
	PasswordHash string `orm:"name:password_hash,comment:bcrypt 密码哈希,length:72,notNull"`
	RegisterDate int64  `orm:"name:register_date,comment:注册时间（毫秒时间戳）"`
	LoginDate    int64  `orm:"name:login_date,comment:最近登录时间（毫秒时间戳）"`
}

func (that *Account) snapshot() comm.AccountSnap {
	return comm.AccountSnap{
		UID:          that.UID,
		PasswordHash: that.PasswordHash,
		RegisterDate: that.RegisterDate,
		LoginDate:    that.LoginDate,
	}
}

// AccountStore 是 contract.IAccountStore 的 Norm 实现。
type AccountStore struct{}

// NewAccountStore 建账号存储，并在这里触发一次建表。
//
// 拿一个空壳对象调 Init 只是为了让 AutoMigrate 在**进程启动时**跑掉：
// 否则第一个注册请求会在 actor 的事件循环里建表，几百毫秒的 DDL
// 会把那个分片的队列堵住，而且失败时是 panic，落在事件循环上更难查。
func NewAccountStore() *AccountStore {
	(&Account{}).Init()
	return &AccountStore{}
}

// Find 按 UID 取账号。Redis 命中就不查 MySQL。
func (that *AccountStore) Find(uid string) (comm.AccountSnap, error) {
	acc := &Account{UID: uid}
	acc.Init()
	if err := acc.Load(); err != nil {
		if orm.IsNotFound(err) {
			return comm.AccountSnap{}, contract.ErrAccountNotFound
		}
		return comm.AccountSnap{}, err
	}
	return acc.snapshot(), nil
}

// Create 建号。
//
// 这里的查重是**第二道**，第一道在 AuthMod 里（按手机号分片，同号串行）。
// 两道都在应用层，是因为 Norm 这条路径上没有第三道：Save 走的是
// INSERT ... ON DUPLICATE KEY UPDATE 且异步执行，唯一冲突既不会报错、
// 也没有返回值可接——重复注册的表现是**后者覆盖前者的密码哈希**，
// 而不是原来那样返回一个 1062 错误。
//
// 多实例部署时仍有一个很窄的窗口：两个进程的分片各自独立，都可能通过查重。
// 但窗口被 Redis 收窄了——Save 写 Redis 是同步的，而所有实例共用同一个 Redis，
// 所以 A 的 Save 返回之后 B 的 Find 就一定看得见。剩下的只有
// "B 查完但 A 还没写完"这一小段。示例项目按单实例部署，这一点如实记在 README 里。
func (that *AccountStore) Create(uid, passwordHash string) (comm.AccountSnap, error) {
	switch _, err := that.Find(uid); {
	case err == nil:
		return comm.AccountSnap{}, contract.ErrPhoneTaken
	case errors.Is(err, contract.ErrAccountNotFound):
		// 正常路径，继续建号
	default:
		return comm.AccountSnap{}, err
	}

	now := time.Now().UnixMilli()
	acc := &Account{
		UID:          uid,
		PasswordHash: passwordHash,
		RegisterDate: now,
		LoginDate:    now,
	}
	acc.Init()
	acc.Save() // 同步写 Redis + 异步入队 MySQL
	return acc.snapshot(), nil
}

// TouchLogin 记一次登录时间。
//
// 必须先 Load 再改再 Save：Save 落的是整行快照，拿一个只填了 UID 和
// LoginDate 的对象去 Save，会把 password_hash 写成空串。
func (that *AccountStore) TouchLogin(uid string, at time.Time) error {
	acc := &Account{UID: uid}
	acc.Init()
	if err := acc.Load(); err != nil {
		if orm.IsNotFound(err) {
			return contract.ErrAccountNotFound
		}
		return err
	}
	acc.LoginDate = at.UnixMilli()
	acc.Save()
	return nil
}
