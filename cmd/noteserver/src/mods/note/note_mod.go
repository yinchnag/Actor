// Package note 是笔记功能模块。
//
// 它是一个 **Mod**（挂在用户身上）而不是 **Mgr**（挂在服务器上）：判断标准是
// "没有用户时这个功能是否也需要正常运行"。笔记不需要——没人在线时它没有任何
// 事情可做。所以文件叫 note_mod.go，类型叫 NoteMod，由 Hub 在**用户登入成功
// 之后**给他挂上，空闲一段时间再回收。
//
// 对照 mods/auth：登入验证在用户登进来之前就得能跑，所以那个是 AuthMgr。
package note

import (
	"time"

	"noteserver/src/comm"
	"noteserver/src/contract"

	"actor"
)

//go:generate go -C ../../../../.. run ./cmd/gercmd gen -force -tmpl cmd/noteserver/templates/user_export.tmpl -recv Hub cmd/noteserver/src/mods/note/note_mod.go cmd/noteserver/src/service

// NoteMod 管一个用户的笔记。每个在线用户一个 actor，一个 actor 一个 NoteMod。
//
// 为什么值得为每个用户开一个 actor：cache 是可变状态，而它被单条协程独占——
// 该用户的所有读写都排在同一个事件循环里，所以这里不需要任何锁，
// 也不可能出现"多设备同时上传导致缓存与数据库不一致"。
// 换成共享的 map + 锁能达到同样的正确性，但每个字段都要自己想清楚锁边界；
// 交给 actor 之后这类问题从代码里消失了。
//
// 代价也要说清楚：存储调用跑在事件循环上，会把这个用户的后续请求排队。
// 对单用户而言这正是想要的语义（他自己的操作本就该有序），
// 而且慢的只是他一个人——这也是"每个用户一个 actor"相对"全局一个 actor"的关键好处。
//
// 注意它的方法签名里没有 uid：这个 actor 只服务一个账号，uid 在构造时就定死了。
// 门面那一层才需要 uid——它得靠 uid 找到该用哪个 actor（见生成的 note_export.go）。
//
// 本模块没有 note_type.go：它的数据形状和跨模块用的 comm.NoteSnap 完全一致，
// 再造一个模块私有类型只是抄字段。真有独属于本模块的类型时才建那个文件，
// 且一旦发现别人也要用，就去 comm 建 **_comm.go 定义 **Snap 对外顶替。
type NoteMod struct {
	actor.ModObj[*NoteMod]

	notes contract.INoteStore
	uid   string

	cache  []comm.NoteSnap // 最近 comm.NoteListLimit 条，按时间倒序
	loaded bool            // cache 是否已经从存储预热过
}

// NewNoteMod 建模块。uid 决定这个 actor 服务哪个账号。
//
// 返回 actor.IModule 的理由同 NewAuthMgr：不给调用方绕过 actor 直接调方法的机会。
func NewNoteMod(notes contract.INoteStore, uid string) actor.IModule {
	m := &NoteMod{notes: notes, uid: uid}
	m.Init()
	return m
}

// Add 存一条笔记，返回落库后的完整快照。上传时间由服务端生成，不采信客户端传来的时间。
//
//	export: NoteAdd
func (that *NoteMod) Add(content string) (comm.NoteSnap, error) {
	n, err := that.notes.Insert(that.uid, content, time.Now())
	if err != nil {
		return comm.NoteSnap{}, err
	}

	// 落库成功才动缓存。顺序反了的话，插入失败会留下一条存储里没有的笔记，
	// 而且要等这个 actor 被回收才会消失。
	//
	// 还有一层：Norm 的 Save 只同步写 Redis，MySQL 是异步的。也就是说
	// "落库成功"在这里的含义是"已经进了 Redis 和存档队列"，
	// 比原来的 INSERT 返回要弱——但对缓存一致性来说够了，
	// 因为后续的 List 走的也是同一套存储。
	if that.loaded {
		that.cache = append([]comm.NoteSnap{n}, that.cache...)
		if len(that.cache) > comm.NoteListLimit {
			that.cache = that.cache[:comm.NoteListLimit]
		}
	}
	return n, nil
}

// List 取笔记，按上传时间倒序。首次访问从存储预热缓存，之后直接命中。
//
//	export: NoteList
func (that *NoteMod) List() ([]comm.NoteSnap, error) {
	if !that.loaded {
		notes, err := that.notes.List(that.uid, comm.NoteListLimit)
		if err != nil {
			return nil, err
		}
		that.cache = notes
		that.loaded = true
	}

	// 拷一份再返回：cache 归这个 actor 独占，直接把切片交出去，
	// 调用方（HTTP 协程）就和事件循环共享了同一块底层数组，
	// 下一次 Add 往里追加就是数据竞争。
	out := make([]comm.NoteSnap, len(that.cache))
	copy(out, that.cache)
	return out, nil
}
