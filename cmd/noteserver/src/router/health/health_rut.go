// Package health 是探活接口。
package health

import (
	"net/http"

	"noteserver/src/bases"

	"github.com/gin-gonic/gin"

	"web"
)

// Health 持有路由需要的依赖。
type HealthRut struct {
	web.Router[*HealthRut]
	hub service
}

// service 是 Health 用到的 Hub 能力。
type service interface {
	OnlineUsers() int
	DiscardedErrors() uint64
}

// New 建路由并挂到 group 上。
func New(group gin.IRoutes, hub service) *HealthRut {
	h := &HealthRut{hub: hub}
	h.Init(group, bases.RouterOpts()...)
	return h
}

// Status 报当前状态。→ GET /healthz
func (that *HealthRut) Status(_ *StatusRequest, ctx *gin.Context) {
	// 两个数都值得盯：online_users 等于当前存活的用户事件循环数，
	// 是最直观的容量指标；discarded 正常应当恒为 0，一旦开始涨，
	// 说明有无返回值的调用在失败——那类失败没有调用方能接，
	// 不在这里露头就彻底看不见了。
	bases.JSON(ctx, http.StatusOK, gin.H{
		"online_users": that.hub.OnlineUsers(),
		"discarded":    that.hub.DiscardedErrors(),
	})
}
