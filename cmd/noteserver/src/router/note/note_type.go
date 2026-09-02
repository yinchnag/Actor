package note

import (
	"noteserver/src/comm"

	"web"
)

// ListRequest 取笔记列表。
//
// path tag 是必需的：两个接口共用 /notes 这一个路径，只靠动词区分。
// 不写 tag 的话会按方法名推成 /list 和 /upload，接口就变了。
//
// 除了标记之外没有字段，框架因此不会尝试绑定任何参数。
type ListRequest struct {
	web.GET `path:"/notes"`
}

// UploadRequest 上传笔记。→ POST /notes
type UploadRequest struct {
	web.POST `path:"/notes"`
	Content  string `json:"content"`
}

// ListResponse 笔记列表应答。
//
// count 是本次返回的条数，不是总数——列表最多返回 comm.NoteListLimit 条，
// 客户端不该拿它当"我一共有多少条笔记"用。
type ListResponse struct {
	Count int             `json:"count"`
	Notes []comm.NoteSnap `json:"notes"`
}
