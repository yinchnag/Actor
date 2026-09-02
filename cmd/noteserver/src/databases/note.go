package databases

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"noteserver/src/comm"

	"github.com/norm/orm"
)

// Note 笔记表。
//
// 字段顺序不是随手排的：uid 和 created_at 挂在同一个索引名 idx_uid_created 上，
// 而 Norm 按**字段声明顺序**组织联合索引的列顺序。写成 (uid, created_at) 才能
// 覆盖"某用户最近 N 条"这个唯一的查询模式；反过来写就退化成全表扫。
//
// Content 不带 length 标记，Norm 会把它建成 TEXT（65535 字节）。
// 上限因此落在 comm.MaxNoteRunes = 10000 上，那里有账。
type Note struct {
	orm.TableSchema[*Note]
	NoteID    string `orm:"primary,name:note_id,comment:笔记唯一ID,length:64,notNull"`
	UID       string `orm:"name:uid,comment:所属账号,length:32,notNull,index:idx_uid_created"`
	CreatedAt int64  `orm:"name:created_at,comment:上传时间（毫秒时间戳）,index:idx_uid_created"`
	Content   string `orm:"name:content,comment:笔记正文"`
}

func (that *Note) snapshot() comm.NoteSnap {
	return comm.NoteSnap{
		NoteID:    that.NoteID,
		Content:   that.Content,
		CreatedAt: that.CreatedAt,
	}
}

// NoteStore 是 contract.INoteStore 的 Norm 实现。
type NoteStore struct {
	// proto 是一个只用来发查询的空壳对象。
	//
	// FindAll 是 TableSchema 上的方法，必须有一个 Init 过的接收者才能拿到
	// TableMeta 和连接池。每次查询新建一个对象也能work，但那要多跑一次
	// sync.Once 和一次反射取 meta；查询本身不碰它的字段，共用一个就够了。
	proto *Note
}

// NewNoteStore 建笔记存储，顺带在启动时建表。
func NewNoteStore() *NoteStore {
	p := &Note{}
	p.Init()
	return &NoteStore{proto: p}
}

// Insert 存一条笔记。
//
// 主键在应用层生成，理由同 Account：Norm 的异步 Save 拿不回自增 ID。
// 格式是 uid-毫秒时间戳-随机后缀，好处是主键本身可读、可按时间粗排，
// 且 Norm 按 hash(pk) 把写入路由到固定 worker——同一用户的笔记会散到不同
// worker 上并行落盘，不会互相排队。
func (that *NoteStore) Insert(uid, content string, createdAt time.Time) (comm.NoteSnap, error) {
	id, err := newNoteID(uid, createdAt)
	if err != nil {
		return comm.NoteSnap{}, err
	}
	n := &Note{
		NoteID: id,
		UID:    uid,
		// 截到毫秒：列里存的就是毫秒时间戳，留着更高精度只会让返回给
		// 客户端的时间和库里存的对不上。
		CreatedAt: createdAt.UnixMilli(),
		Content:   content,
	}
	n.Init()
	n.Save()
	return n.snapshot(), nil
}

// List 取某账号最近 limit 条笔记，按上传时间倒序。
//
// 走的是 MySQL，不吃 Redis 缓存——Norm 的缓存是按主键的，"某用户的笔记列表"
// 不是一个主键。真正的缓存在上一层：NoteMod 把结果留在 actor 里，
// 一个在线用户只会打到这里一次。
func (that *NoteStore) List(uid string, limit int) ([]comm.NoteSnap, error) {
	cond, err := eqCond("uid", uid)
	if err != nil {
		return nil, err
	}
	// 排序带上 note_id：同一毫秒内上传的多条否则没有确定顺序，
	// 翻页和"刚上传的那条在不在最前面"都会随机抖动。
	rows, err := that.proto.FindAll(cond, "`created_at` DESC, `note_id` DESC", limit)
	if err != nil {
		return nil, err
	}
	out := make([]comm.NoteSnap, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.snapshot())
	}
	return out, nil
}

// newNoteID 生成笔记主键：uid-毫秒时间戳-6字节随机。
//
// 随机后缀不能省。同一个用户的笔记写在他自己的 actor 上是串行的，
// 但毫秒级时间戳在连续两次上传里可能相同，那样第二条的 Save 会
// ON DUPLICATE KEY UPDATE 覆盖掉第一条——静默丢数据，最难查的那种。
func newNoteID(uid string, at time.Time) (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成笔记ID失败: %w", err)
	}
	return fmt.Sprintf("%s-%013d-%s", uid, at.UnixMilli(), hex.EncodeToString(buf[:])), nil
}
