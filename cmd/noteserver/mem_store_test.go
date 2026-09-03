package main

import (
	"sort"
	"sync"
	"time"

	"noteserver/src/comm"
	"noteserver/src/contract"
)

// --- 内存存储：让 actor 编排和 HTTP 层能在没有 MySQL/Redis 的环境里被完整测到 ---
//
// 这就是 contract 包存在的理由。生产环境注入 databases 里的 Norm 实现，
// 测试注入这里的 map——两边实现的是同一组接口，所以被测的是真正跑在线上的
// 那套 actor 编排、HTTP 路由和错误翻译，而不是一套为测试特制的旁路。

type memAccounts struct {
	mu     sync.Mutex // 多个 auth 分片会并发进来，必须加锁
	data   map[string]comm.AccountSnap
	onCall func(op string) error // 注入故障用
}

func newMemAccounts() *memAccounts {
	return &memAccounts{data: make(map[string]comm.AccountSnap)}
}

func (that *memAccounts) fail(op string) error {
	if that.onCall == nil {
		return nil
	}
	return that.onCall(op)
}

func (that *memAccounts) Find(uid string) (comm.AccountSnap, error) {
	that.mu.Lock()
	defer that.mu.Unlock()
	if err := that.fail("Find"); err != nil {
		return comm.AccountSnap{}, err
	}
	acc, ok := that.data[uid]
	if !ok {
		return comm.AccountSnap{}, contract.ErrAccountNotFound
	}
	return acc, nil
}

func (that *memAccounts) Create(uid, hash string) (comm.AccountSnap, error) {
	that.mu.Lock()
	defer that.mu.Unlock()
	if err := that.fail("Create"); err != nil {
		return comm.AccountSnap{}, err
	}
	if _, ok := that.data[uid]; ok {
		return comm.AccountSnap{}, contract.ErrPhoneTaken
	}
	now := time.Now().UnixMilli()
	acc := comm.AccountSnap{UID: uid, PasswordHash: hash, RegisterDate: now, LoginDate: now}
	that.data[uid] = acc
	return acc, nil
}

func (that *memAccounts) TouchLogin(uid string, at time.Time) error {
	that.mu.Lock()
	defer that.mu.Unlock()
	acc, ok := that.data[uid]
	if !ok {
		return contract.ErrAccountNotFound
	}
	acc.LoginDate = at.UnixMilli()
	that.data[uid] = acc
	return nil
}

type memNotes struct {
	mu     sync.Mutex
	data   map[string][]comm.NoteSnap
	seq    int64
	onCall func(op string) error
}

func newMemNotes() *memNotes {
	return &memNotes{data: make(map[string][]comm.NoteSnap)}
}

func (that *memNotes) fail(op string) error {
	if that.onCall == nil {
		return nil
	}
	return that.onCall(op)
}

func (that *memNotes) Insert(uid, content string, at time.Time) (comm.NoteSnap, error) {
	that.mu.Lock()
	defer that.mu.Unlock()
	if err := that.fail("Insert"); err != nil {
		return comm.NoteSnap{}, err
	}
	that.seq++
	// 主键形态跟 databases.newNoteID 对齐：uid-时间戳-唯一后缀。
	// 后缀这里用自增而不是随机，是为了让下面的排序有确定结果。
	n := comm.NoteSnap{
		NoteID:    uid + "-" + itoa64(at.UnixMilli()) + "-" + itoa64(that.seq),
		Content:   content,
		CreatedAt: at.UnixMilli(),
	}
	that.data[uid] = append(that.data[uid], n)
	return n, nil
}

func (that *memNotes) List(uid string, limit int) ([]comm.NoteSnap, error) {
	that.mu.Lock()
	defer that.mu.Unlock()
	if err := that.fail("List"); err != nil {
		return nil, err
	}
	src := that.data[uid]
	out := make([]comm.NoteSnap, len(src))
	copy(out, src)
	// 与 MySQL 的 ORDER BY created_at DESC, note_id DESC 保持一致
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].NoteID > out[j].NoteID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (that *memNotes) count(uid string) int {
	that.mu.Lock()
	defer that.mu.Unlock()
	return len(that.data[uid])
}

type memSessions struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemSessions() *memSessions { return &memSessions{data: make(map[string]string)} }

func (that *memSessions) Put(token, uid string, _ time.Duration) error {
	that.mu.Lock()
	defer that.mu.Unlock()
	that.data[token] = uid
	return nil
}

func (that *memSessions) Get(token string) (string, error) {
	that.mu.Lock()
	defer that.mu.Unlock()
	uid, ok := that.data[token]
	if !ok {
		return "", contract.ErrNoSession
	}
	return uid, nil
}

func (that *memSessions) Delete(token string) error {
	that.mu.Lock()
	defer that.mu.Unlock()
	delete(that.data, token)
	return nil
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

type memMails struct {
	mu     sync.Mutex
	data   map[string][]comm.MailSnap
	onCall func(op string) error
	// loads 记录 Load 被调了几次。用来验证"用户侧的读不回 MailMgr"——
	// 那是给用户加一个 MailboxMod 的全部理由，没有这个计数就只能靠肉眼相信。
	loads int
}

func newMemMails() *memMails { return &memMails{data: make(map[string][]comm.MailSnap)} }

func (that *memMails) fail(op string) error {
	if that.onCall == nil {
		return nil
	}
	return that.onCall(op)
}

func (that *memMails) Load(uid string) ([]comm.MailSnap, error) {
	that.mu.Lock()
	defer that.mu.Unlock()
	that.loads++
	if err := that.fail("Load"); err != nil {
		return nil, err
	}
	// 拷一份再给：真实存储每次都返回新对象，内存版直接把 map 里的切片交出去
	// 会让调用方改到"存储里的数据"，测试就验不出"忘了写回"这类 bug。
	out := make([]comm.MailSnap, len(that.data[uid]))
	copy(out, that.data[uid])
	return out, nil
}

func (that *memMails) Save(uid string, mails []comm.MailSnap) error {
	that.mu.Lock()
	defer that.mu.Unlock()
	if err := that.fail("Save"); err != nil {
		return err
	}
	cp := make([]comm.MailSnap, len(mails))
	copy(cp, mails)
	that.data[uid] = cp
	return nil
}

func (that *memMails) count(uid string) int {
	that.mu.Lock()
	defer that.mu.Unlock()
	return len(that.data[uid])
}

func (that *memMails) loadCount() int {
	that.mu.Lock()
	defer that.mu.Unlock()
	return that.loads
}

// snapshot 取某个用户存储里的邮件副本，供测试构造场景用。
func (that *memMails) snapshot(uid string) []comm.MailSnap {
	that.mu.Lock()
	defer that.mu.Unlock()
	out := make([]comm.MailSnap, len(that.data[uid]))
	copy(out, that.data[uid])
	return out
}
