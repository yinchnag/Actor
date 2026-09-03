package comm

// NoteSnap 是一条笔记的跨模块快照，同时也是直接序列化给客户端的形状。
//
// json tag 写在这里而不是另建一套响应类型：笔记的对外形状和跨模块形状
// 恰好一致，多一层转换只是抄字段。要是哪天两者分了岔（比如对外要隐藏
// 某个字段），就在 router 的 **_type.go 里加一个 **Response 承接。
type NoteSnap struct {
	NoteID    string `json:"note_id"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"` // 毫秒时间戳
}

// 笔记功能的常量。只有 note 这一个功能在用。
const (
	// MaxNoteRunes 单条笔记的字数上限。
	//
	// 需求是"至少能存 800 汉字"。这里给到 10000，理由是 Norm 把没有 length
	// 标记的 string 字段映射成 MySQL 的 TEXT（65535 字节）——10000 个字符
	// 即使全是 4 字节的 emoji 也只有 40000 字节，离上限还有一半余量。
	// 想再往上加就必须先解决列类型：TEXT 装不下 20000 个 4 字节字符。
	MaxNoteRunes = 10000

	// NoteListLimit 一次返回的最大笔记条数，也是 NoteMod 缓存持有的条数。
	NoteListLimit = 200
)
