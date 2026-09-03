package databases

import (
	"noteserver/src/comm"

	"github.com/norm/orm"
)

// Mailbox 是一个用户的整个信箱，一条记录装下他所有的邮件。
//
// 为什么不是"一封邮件一行"：
//
//   - 需求里的"最多 100 封，超了顶掉最老的"是一次**读改写**。拆成行的话，
//     每次下发都要 FindAll 一遍、算出该删哪封、再发一次删除——三次存储往返，
//     而且 Norm 的删除是异步软删，中间那个窗口里信箱可能是 101 封。
//     整个信箱一条记录时，这一步就是切片操作，由 mail actor 的事件循环串行保证。
//   - Norm 把 slice 字段自动序列化成 JSON 列，正好装得下。
//   - 读取信箱是"按 UID 取一条"，Redis 命中就不查 MySQL——而按行存的话
//     "某用户的邮件列表"不是一个主键，缓存帮不上忙。
//
// 代价说清楚：单条记录会随邮件数增长。上限乘以 comm.MailKeepLimit 是
// 100 × (64 + 2000) 字符，最坏 ~800KB，在 MySQL 的 JSON 列（上限 1GB）里够用。
// 真要支持"无上限归档邮件"就得改成按行存，那是另一个需求。
type Mailbox struct {
	orm.TableSchema[*Mailbox]
	UID   string          `orm:"primary,name:uid,comment:账号唯一ID（手机号）,length:32,notNull"`
	Mails []comm.MailSnap `orm:"name:mails,comment:邮件列表（按下发时间倒序）"`
}

// MailStore 是 contract.IMailStore 的 Norm 实现。
type MailStore struct{}

// NewMailStore 建邮件存储，并在这里触发一次建表。
func NewMailStore() *MailStore {
	(&Mailbox{}).Init()
	return &MailStore{}
}

// Load 取某个用户的信箱。从没收过邮件时返回空切片，不返回错误。
func (that *MailStore) Load(uid string) ([]comm.MailSnap, error) {
	box := &Mailbox{UID: uid}
	box.Init()
	if err := box.Load(); err != nil {
		if orm.IsNotFound(err) {
			return []comm.MailSnap{}, nil // 从没收过邮件，不是错误
		}
		return nil, err
	}
	if box.Mails == nil {
		return []comm.MailSnap{}, nil
	}
	return box.Mails, nil
}

// Save 整体写回信箱。
//
// 必须整体写：Norm 的 Save 落的是整行快照，拿一个只填了部分字段的对象去写，
// 没填的那些会被写成零值。
func (that *MailStore) Save(uid string, mails []comm.MailSnap) error {
	box := &Mailbox{UID: uid, Mails: mails}
	box.Init()
	box.Save() // 同步写 Redis + 异步入队 MySQL
	return nil
}
