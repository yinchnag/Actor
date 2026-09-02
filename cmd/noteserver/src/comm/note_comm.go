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
