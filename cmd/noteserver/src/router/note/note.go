// Package note 是上传与获取笔记两个接口。
package note

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"noteserver/src/bases"
	"noteserver/src/comm"
	"noteserver/src/contract"
	"noteserver/src/middleware"

	"github.com/gin-gonic/gin"

	"web"

	"actor"
)

// Note 持有路由需要的依赖。
type Note struct {
	web.Router[*Note]
	hub service
}

// service 是 Note 用到的 Hub 能力。
//
// 两个门面方法由 gercmd 生成（src/service/note_export.go）。注意这里已经
// 没有 AcquireUser/ReleaseUser 了——取用与归还被收进了生成的门面，
// 不再是"handler 要记得写 defer"的事。漏一次 Release 那个 actor 就永远
// 不会被回收，这种错误不该靠人守。
type service interface {
	NoteAdd(gid uint64, uid string, content string) (contract.NoteInfo, error)
	NoteList(gid uint64, uid string) ([]contract.NoteInfo, error)
}

// New 建路由并挂到 group 上。
//
// group 应当已经套好鉴权中间件——两个接口都要求登录。中间件仍然由调用方
// 显式套在分组上，没有做成"标记里再加一个 auth tag"：谁需要登录是装配期的
// 部署决定，藏进请求类型里反而更难看出来。
func New(group gin.IRoutes, hub service) *Note {
	n := &Note{hub: hub}
	n.Init(group, bases.RouterOpts()...)
	return n
}

// List 取笔记，按上传时间倒序。→ GET /notes
func (that *Note) List(_ *ListRequest, ctx *gin.Context) {
	uid := middleware.UID(ctx)

	notes, err := that.hub.NoteList(actor.CurrentGID(), uid)
	if err != nil {
		bases.FailInvoke(ctx, err, "获取笔记")
		return
	}
	bases.JSON(ctx, http.StatusOK, ListResponse{Count: len(notes), Notes: notes})
}

// Upload 上传一条笔记。→ POST /notes
func (that *Note) Upload(req *UploadRequest, ctx *gin.Context) {
	uid := middleware.UID(ctx)

	// 只裁首尾空白，中间的换行和空格是笔记内容的一部分，不能动
	content := strings.TrimSpace(req.Content)
	if content == "" {
		bases.Fail(ctx, http.StatusBadRequest, "笔记内容不能为空")
		return
	}
	if !utf8.ValidString(content) {
		bases.Fail(ctx, http.StatusBadRequest, "笔记内容不是合法的 UTF-8")
		return
	}
	// 按字数而不是字节数限制：800 个汉字在 UTF-8 里是 2400 字节，
	// 按字节算会让中文用户能写的字数只有英文用户的三分之一。
	if n := utf8.RuneCountInString(content); n > comm.MaxNoteRunes {
		bases.Fail(ctx, http.StatusRequestEntityTooLarge,
			"笔记最多 "+strconv.Itoa(comm.MaxNoteRunes)+" 字，当前 "+strconv.Itoa(n)+" 字")
		return
	}

	note, err := that.hub.NoteAdd(actor.CurrentGID(), uid, content)
	if err != nil {
		bases.FailInvoke(ctx, err, "上传笔记")
		return
	}
	bases.JSON(ctx, http.StatusCreated, note)
}
