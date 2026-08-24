package main

import (
	"context"
	"time"

	"actor"
)

// noteListLimit 是一次返回的最大条数，也是缓存持有的条数。
const noteListLimit = 200

// NoteMod 管一个用户的笔记。每个在线用户一个 actor，一个 actor 一个 NoteMod。
//
// 为什么值得为每个用户开一个 actor：cache 是可变状态，而它被单条协程独占——
// 该用户的所有读写都排在同一个事件循环里，所以这里不需要任何锁，
// 也不可能出现"多设备同时上传导致缓存与数据库不一致"。
// 换成共享的 map + 锁能达到同样的正确性，但每个字段都要自己想清楚锁边界；
// 交给 actor 之后这类问题从代码里消失了。
//
// 代价也要说清楚：数据库调用跑在事件循环上，会把这个用户的后续请求排队。
// 对单用户而言这正是想要的语义（他自己的操作本就该有序），
// 而且慢的只是他一个人——这也是"每个用户一个 actor"相对"全局一个 actor"的关键好处。
type NoteMod struct {
	actor.ModObj[*NoteMod]

	store  Store
	userID int64

	cache  []Note // 最近 noteListLimit 条，按时间倒序
	loaded bool   // cache 是否已经从数据库预热过
}

func NewNoteMod(store Store, userID int64) *NoteMod {
	m := &NoteMod{store: store, userID: userID}
	m.Init()
	return m
}

type AddNoteArgs struct {
	Content string
}

type AddNoteResult struct {
	Note Note
}

// Add 存一条笔记。上传时间由服务端生成，不采信客户端传来的时间。
func (m *NoteMod) Add(a AddNoteArgs) (AddNoteResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	// 截到毫秒：MySQL 建表用的是 DATETIME(3)，留着更高精度只会让
	// 返回给客户端的时间和库里存的对不上。
	createdAt := time.Now().Truncate(time.Millisecond)

	id, err := m.store.InsertNote(ctx, m.userID, a.Content, createdAt)
	if err != nil {
		return AddNoteResult{}, err
	}
	n := Note{ID: id, Content: a.Content, CreatedAt: createdAt}

	// 落库成功才动缓存。顺序反了的话，插入失败会留下一条数据库里没有的笔记，
	// 而且要等这个 actor 被回收才会消失。
	if m.loaded {
		m.cache = append([]Note{n}, m.cache...)
		if len(m.cache) > noteListLimit {
			m.cache = m.cache[:noteListLimit]
		}
	}
	return AddNoteResult{Note: n}, nil
}

type ListNotesArgs struct{}

type ListNotesResult struct {
	Notes []Note
}

// List 取笔记，按上传时间倒序。首次访问从数据库预热缓存，之后直接命中。
func (m *NoteMod) List(_ ListNotesArgs) (ListNotesResult, error) {
	if !m.loaded {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()

		notes, err := m.store.ListNotes(ctx, m.userID, noteListLimit)
		if err != nil {
			return ListNotesResult{}, err
		}
		m.cache = notes
		m.loaded = true
	}

	// 拷一份再返回：cache 归这个 actor 独占，直接把切片交出去，
	// 调用方（HTTP 协程）就和事件循环共享了同一块底层数组，
	// 下一次 Add 往里追加就是数据竞争。
	out := make([]Note, len(m.cache))
	copy(out, m.cache)
	return ListNotesResult{Notes: out}, nil
}
