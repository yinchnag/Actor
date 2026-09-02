// Package auth 是注册与登录两个接口。
package auth

import (
	"errors"
	"net/http"
	"time"

	"noteserver/src/bases"
	"noteserver/src/comm"
	"noteserver/src/contract"
	"noteserver/src/security"

	"github.com/gin-gonic/gin"

	"web"

	"actor"
)

// Auth 持有路由需要的依赖。
//
// 内嵌 web.Router[*Auth] 之后，下面两个公有方法会被 Init 反射扫出来
// 并自动挂到 gin 上；类型参数必须写成自己，写错了 Init 会当场 panic。
type Auth struct {
	web.Router[*Auth]
	hub service
}

// service 是 Auth 用到的 Hub 能力，抽成接口只是为了让这个文件不必
// 反向依赖 service 包的全部内容——真正的实现就是 *service.Hub。
//
// 三个门面方法都是 gercmd 从模块方法上的 export: 标记生成的
// （src/service/auth_export.go）。路由因此不再碰 loader，也不再出现
// "AuthMod"/"Register" 这类字符串——改了模块方法名，这里编译就红。
type service interface {
	AuthRegister(gid uint64, uid string, passwordHash string) (string, error)
	AuthLookup(gid uint64, uid string) (contract.AccountInfo, error)
	AuthTouchLogin(gid uint64, uid string, at time.Time)
	Sessions() contract.ISessionStore
}

// New 建路由并把接口挂到 group 上。
//
// 挂哪几条、什么方法、什么路径，全部由下面的方法签名和请求类型决定，
// 这里不再逐条 group.POST。加一个接口 = 加一个方法 + 一个请求类型。
func New(group gin.IRoutes, hub service) *Auth {
	a := &Auth{hub: hub}
	a.Init(group, bases.RouterOpts()...)
	return a
}

// Register 注册。→ POST /register
func (that *Auth) Register(req *RegisterRequest, ctx *gin.Context) {
	if !security.ValidPhone(req.Phone) {
		bases.Fail(ctx, http.StatusBadRequest, "手机号格式不正确")
		return
	}
	if msg, ok := security.CheckPassword(req.Password); !ok {
		bases.Fail(ctx, http.StatusBadRequest, msg)
		return
	}

	// bcrypt 在这里算，不进 actor：它是几十毫秒的纯 CPU 活，
	// 跑在事件循环上会把整个分片钉死那么久。
	hash, err := security.HashPassword(req.Password)
	if err != nil {
		bases.Fail(ctx, http.StatusInternalServerError, "服务器内部错误")
		return
	}

	// 每个请求只取一次 GID，之后该请求内所有 actor 调用复用它。
	// ModInvoke 每次都要解析调用栈（约 1.7µs），比投递本身还贵。
	gid := actor.CurrentGID()

	uid, err := that.hub.AuthRegister(gid, req.Phone, hash)
	if err != nil {
		if errors.Is(err, contract.ErrPhoneTaken) {
			bases.Fail(ctx, http.StatusConflict, "手机号已被注册")
			return
		}
		bases.FailInvoke(ctx, err, "注册")
		return
	}
	bases.JSON(ctx, http.StatusCreated, RegisterResponse{UID: uid})
}

// Login 登录。→ POST /login
func (that *Auth) Login(req *LoginRequest, ctx *gin.Context) {
	if req.Phone == "" || req.Password == "" {
		bases.Fail(ctx, http.StatusBadRequest, "手机号和密码不能为空")
		return
	}

	gid := actor.CurrentGID()

	acc, err := that.hub.AuthLookup(gid, req.Phone)

	// 账号不存在也照样跑一次 bcrypt，让两条路径耗时一致，
	// 否则响应时间就是一个"这个号注册过没有"的探测接口。
	found := false
	switch {
	case err == nil:
		found = true
	case errors.Is(err, contract.ErrAccountNotFound):
		// 落到假比对
	default:
		bases.FailInvoke(ctx, err, "登录")
		return
	}

	if !security.ComparePassword(acc.PasswordHash, req.Password, found) {
		// 不区分"账号不存在"和"密码错误"，避免泄露号码是否注册过
		bases.Fail(ctx, http.StatusUnauthorized, "手机号或密码错误")
		return
	}

	token, err := security.NewToken()
	if err != nil {
		bases.Fail(ctx, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if err := that.hub.Sessions().Put(token, acc.UID, comm.SessionTTL); err != nil {
		bases.Fail(ctx, http.StatusServiceUnavailable, "会话服务暂时不可用")
		return
	}

	// 记录登录时间：无返回值 = 投递即忘，登录应答不等它落库。
	// 失败会走 Hub 装的 DiscardedErrorHandler，不会静默消失。
	that.hub.AuthTouchLogin(gid, acc.UID, time.Now())

	bases.JSON(ctx, http.StatusOK, LoginResponse{
		Token:     token,
		UID:       acc.UID,
		ExpiresIn: int(comm.SessionTTL.Seconds()),
	})
}
