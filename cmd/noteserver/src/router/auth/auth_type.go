package auth

import "web"

// RegisterRequest 注册请求体。
//
// 内嵌的 web.POST 就是路由声明：动词是 POST，路径没写 path tag，
// 于是按方法名推出 /register。整个包里没有一处显式的路由注册调用。
type RegisterRequest struct {
	web.POST
	Phone    string `json:"phone"`
	Password string `json:"password"`
}

// LoginRequest 登录请求体。路径同样按方法名推出 /login。
type LoginRequest struct {
	web.POST
	Phone    string `json:"phone"`
	Password string `json:"password"`
}

// RegisterResponse 注册应答。
//
// 返回的 uid 就是手机号——账号主键在换成 Norm 之后由自增 id 改成了手机号，
// 见 databases/account.go。客户端本来就知道自己的手机号，这里没有多泄露什么。
type RegisterResponse struct {
	UID string `json:"uid"`
}

// LoginResponse 登录应答。
type LoginResponse struct {
	Token     string `json:"token"`
	UID       string `json:"uid"`
	ExpiresIn int    `json:"expires_in"` // 秒
}
