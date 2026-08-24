package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"actor"

	"golang.org/x/crypto/bcrypt"
)

const (
	// sessionTTL 登录会话有效期。
	sessionTTL = 7 * 24 * time.Hour

	// maxNoteRunes 单条笔记的字数上限。
	// 需求是"至少能存 800 汉字"，这里给到 20000——建表用 MEDIUMTEXT，
	// 就算全是 4 字节字符也只有 80KB，离 16MB 上限很远，不会截断。
	maxNoteRunes = 20000

	// minPasswordLen / maxPasswordBytes 密码长度约束。
	// 上限 72 是 bcrypt 的硬限制：超过 72 字节的部分会被它直接忽略，
	// 不显式拒掉的话，用户以为自己设了 100 位密码，实际只有前 72 字节生效。
	minPasswordLen   = 8
	maxPasswordBytes = 72

	// maxBodyBytes 请求体上限，防止有人拿超大 body 打内存。
	// 按 maxNoteRunes 全 4 字节字符加 JSON 转义留的余量。
	maxBodyBytes = 1 << 20
)

// 中国大陆手机号。需求只要求"必须是手机号"、不要求短信验证，
// 所以这里只做格式校验。要支持国际号码就改这个正则。
var phonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)

// dummyHash 是一个固定的 bcrypt 哈希，用于账号不存在时的假比对。
// 不这么做的话，"账号不存在"会立刻返回，而"密码错误"要花几十毫秒算 bcrypt，
// 攻击者靠响应时间就能枚举出哪些手机号注册过。
var dummyHash []byte

func init() {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)
	if err != nil {
		panic(err) // 只在 bcrypt 参数非法时发生，属于编程错误
	}
	dummyHash = h
}

func logDiscarded(e actor.DiscardedError) {
	log.Printf("[discarded] %v", e)
}

// Server 把 actor 编排、会话存储和 HTTP 路由绑在一起。
type Server struct {
	hub      *Hub
	sessions SessionStore
}

func NewServer(hub *Hub, sessions SessionStore) *Server {
	return &Server{hub: hub, sessions: sessions}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", s.handleRegister)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/notes", s.handleNotes)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"online_users": s.hub.OnlineUsers()})
	})
	return mux
}

// --- 注册 ---

type registerRequest struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !phonePattern.MatchString(req.Phone) {
		writeError(w, http.StatusBadRequest, "手机号格式不正确")
		return
	}
	if msg, ok := checkPassword(req.Password); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// bcrypt 在这里算，不进 actor：它是几十毫秒的纯 CPU 活，
	// 跑在事件循环上会把整个分片钉死那么久。
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}

	// 每个请求只取一次 GID，之后该请求内所有 actor 调用复用它。
	// ModInvoke 每次都要解析调用栈（约 1.7µs），比投递本身还贵。
	gid := actor.CurrentGID()
	auth := s.hub.AuthFor(req.Phone)

	out, err := auth.ModInvokeFrom(gid, "AuthMod", "Register", RegisterArgs{
		Phone:        req.Phone,
		PasswordHash: string(hash),
	})
	res, err := unwrap[RegisterResult](out, err)
	if err != nil {
		if errors.Is(err, ErrPhoneTaken) {
			writeError(w, http.StatusConflict, "手机号已被注册")
			return
		}
		writeInvokeError(w, err, "注册")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user_id": res.UserID})
}

// --- 登录 ---

type loginRequest struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Phone == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "手机号和密码不能为空")
		return
	}

	gid := actor.CurrentGID()
	auth := s.hub.AuthFor(req.Phone)

	out, err := auth.ModInvokeFrom(gid, "AuthMod", "Lookup", LookupArgs{Phone: req.Phone})
	res, err := unwrap[LookupResult](out, err)

	// 账号不存在也照样跑一次 bcrypt，让两条路径耗时一致，
	// 否则响应时间就是一个"这个号注册过没有"的探测接口。
	stored := dummyHash
	found := false
	switch {
	case err == nil:
		stored, found = []byte(res.PasswordHash), true
	case errors.Is(err, ErrUserNotFound):
		// 落到假比对
	default:
		writeInvokeError(w, err, "登录")
		return
	}

	if bcrypt.CompareHashAndPassword(stored, []byte(req.Password)) != nil || !found {
		// 不区分"账号不存在"和"密码错误"，避免泄露号码是否注册过
		writeError(w, http.StatusUnauthorized, "手机号或密码错误")
		return
	}

	token, err := newToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	if err := s.sessions.Put(ctx, token, res.UserID, sessionTTL); err != nil {
		log.Printf("写会话失败: %v", err)
		writeError(w, http.StatusServiceUnavailable, "会话服务暂时不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"user_id":    res.UserID,
		"expires_in": int(sessionTTL.Seconds()),
	})
}

// --- 笔记 ---

type uploadRequest struct {
	Content string `json:"content"`
}

func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listNotes(w, r, userID)
	case http.MethodPost:
		s.uploadNote(w, r, userID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "只支持 GET 和 POST")
	}
}

func (s *Server) listNotes(w http.ResponseWriter, r *http.Request, userID int64) {
	gid := actor.CurrentGID()
	loader := s.hub.AcquireUser(userID)
	defer s.hub.ReleaseUser(userID)

	out, err := loader.ModInvokeFrom(gid, "NoteMod", "List", ListNotesArgs{})
	res, err := unwrap[ListNotesResult](out, err)
	if err != nil {
		writeInvokeError(w, err, "获取笔记")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(res.Notes),
		"notes": res.Notes,
	})
}

func (s *Server) uploadNote(w http.ResponseWriter, r *http.Request, userID int64) {
	var req uploadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// 只裁首尾空白，中间的换行和空格是笔记内容的一部分，不能动
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeError(w, http.StatusBadRequest, "笔记内容不能为空")
		return
	}
	if !utf8.ValidString(content) {
		writeError(w, http.StatusBadRequest, "笔记内容不是合法的 UTF-8")
		return
	}
	// 按字数而不是字节数限制：800 个汉字在 UTF-8 里是 2400 字节，
	// 按字节算会让中文用户能写的字数只有英文用户的三分之一。
	if n := utf8.RuneCountInString(content); n > maxNoteRunes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("笔记最多 %d 字，当前 %d 字", maxNoteRunes, n))
		return
	}

	gid := actor.CurrentGID()
	loader := s.hub.AcquireUser(userID)
	defer s.hub.ReleaseUser(userID)

	out, err := loader.ModInvokeFrom(gid, "NoteMod", "Add", AddNoteArgs{Content: content})
	res, err := unwrap[AddNoteResult](out, err)
	if err != nil {
		writeInvokeError(w, err, "上传笔记")
		return
	}
	writeJSON(w, http.StatusCreated, res.Note)
}

// authenticate 校验 Authorization: Bearer <token>，返回用户 ID。
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (int64, bool) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "缺少 Authorization: Bearer <token>")
		return 0, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	// 会话校验是纯读，没有需要串行化的状态，所以不必绕进 actor。
	userID, err := s.sessions.Get(ctx, token)
	if errors.Is(err, ErrNoSession) {
		writeError(w, http.StatusUnauthorized, "登录已过期，请重新登录")
		return 0, false
	}
	if err != nil {
		log.Printf("读会话失败: %v", err)
		writeError(w, http.StatusServiceUnavailable, "会话服务暂时不可用")
		return 0, false
	}
	return userID, true
}

// --- 辅助 ---

// unwrap 把框架的 ([]reflect.Value, error) 还原成 (T, error)。
//
// 框架的 ModInvoke 返回两层错误：invokeErr 是投递/调度层面的失败，
// out[1] 才是模块方法自己返回的业务错误。两层都得看。
func unwrap[T any](out []reflect.Value, invokeErr error) (T, error) {
	var zero T
	if invokeErr != nil {
		return zero, invokeErr
	}
	if len(out) != 2 {
		return zero, fmt.Errorf("模块方法返回值个数异常: %d", len(out))
	}
	if bizErr, _ := out[1].Interface().(error); bizErr != nil {
		return zero, bizErr
	}
	v, ok := out[0].Interface().(T)
	if !ok {
		return zero, fmt.Errorf("模块方法返回类型异常: %T", out[0].Interface())
	}
	return v, nil
}

// writeInvokeError 把框架层的失败翻译成诚实的 HTTP 语义。
//
// 关键在于区分"确定没执行"和"可能已执行"：前者客户端可以放心重试，
// 后者重试就有重复写入的风险，必须如实告诉它。
func writeInvokeError(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, actor.ErrTaskCanceled):
		// 等待超时，且任务在被取用之前就已取消——模块方法一次都没执行
		log.Printf("%s 超时（已取消，未执行）: %v", op, err)
		writeError(w, http.StatusServiceUnavailable, op+"超时，操作未执行，可以直接重试")

	case errors.Is(err, actor.ErrTaskAwaitTimeout):
		// 同样是超时，但没带取消标记：方法可能正在执行或已执行完
		log.Printf("%s 超时（可能已执行）: %v", op, err)
		writeError(w, http.StatusGatewayTimeout, op+"超时，操作可能已生效，重试前请先确认")

	case errors.Is(err, actor.ErrTaskQueueTimeout):
		log.Printf("%s 排队超时: %v", op, err)
		writeError(w, http.StatusServiceUnavailable, "服务繁忙，请稍后重试")

	case errors.Is(err, actor.ErrCallCycle):
		// 环形同步调用，属于服务端的编排 bug，不该让客户端重试
		log.Printf("%s 出现跨 actor 调用环，这是服务端 bug: %v", op, err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")

	default:
		// 走到这里的多半是 actor 已关闭（框架未导出该哨兵错误，无法用
		// errors.Is 精确判定），或数据库返回的错误。统一按暂时不可用处理。
		log.Printf("%s 失败: %v", op, err)
		writeError(w, http.StatusServiceUnavailable, op+"失败，请稍后重试")
	}
}

func checkPassword(pw string) (string, bool) {
	if len([]rune(pw)) < minPasswordLen {
		return fmt.Sprintf("密码至少 %d 位", minPasswordLen), false
	}
	if len(pw) > maxPasswordBytes {
		return fmt.Sprintf("密码过长（最多 %d 字节）", maxPasswordBytes), false
	}
	return "", true
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("写响应失败: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
