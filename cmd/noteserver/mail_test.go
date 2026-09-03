package main

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"noteserver/src/comm"
)

// sendMail 以运维身份下发一封邮件。
func (that *harness) sendMail(uid, title string, items []map[string]int) (int, map[string]any) {
	that.t.Helper()
	body := map[string]any{"uid": uid, "title": title, "content": "正文", "items": items}
	return that.doOps(http.MethodPost, "/api/mail/send", opsToken, body)
}

// doOps 与 do 的区别只有令牌的含义：这里是运维令牌，不是用户的 Bearer。
// 单独包一层是为了让测试里"以谁的身份发的请求"一眼可见。
func (that *harness) doOps(method, path, token string, body any) (int, map[string]any) {
	that.t.Helper()
	return that.do(method, path, token, body)
}

// listMails 拉取邮件，等到信箱就绪为止。
//
// 数据是登录后由 MailMgr **推**给用户 actor 的，这中间隔着两次跨 actor 投递，
// 所以刚登录就拉可能拿到 ready=false（HTTP 202）。这不是 bug 而是这套
// "只通知不返回"协议的直接后果：请求与结果不在同一次往返里。
// 真实客户端会重试或等服务端推送，测试里就轮询几次。
func (that *harness) listMails(token string) map[string]any {
	that.t.Helper()
	for i := 0; i < 100; i++ {
		code, body := that.do(http.MethodGet, "/api/mails", token, nil)
		if code == http.StatusOK {
			return body
		}
		if code != http.StatusAccepted {
			that.t.Fatalf("拉取邮件失败: code=%d body=%v", code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	that.t.Fatal("等了 1 秒信箱仍未就绪")
	return nil
}

// claim 领取附件。它是**同步**的：rut 直连 MailMgr，返回时存储已经改好了，
// items 就是权威结果。
func (that *harness) claim(token, mailID string) (int, map[string]any) {
	that.t.Helper()
	return that.do(http.MethodPost, "/api/mails/claim", token,
		map[string]string{"mail_id": mailID})
}

// waitClaimed 等用户侧那份视图追上权威状态。
//
// 领取本身是同步的，但用户 actor 上那份缓存是 MailMgr 异步推新的，可能比响应
// 晚一点点到。这是读缓存的固有代价，不是 bug——真实游戏里客户端等的是服务端
// 推送而不是轮询。
func (that *harness) waitClaimed(token, mailID string) {
	that.t.Helper()
	for i := 0; i < 100; i++ {
		box := that.listMails(token)
		mails, _ := box["mails"].([]any)
		for _, raw := range mails {
			m, _ := raw.(map[string]any)
			if m["mail_id"] != mailID {
				continue
			}
			if claimed, _ := m["claimed"].(bool); claimed {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	that.t.Fatal("等了 1 秒，用户侧视图仍未同步到已领取")
}

// TestMailSendAndList 运维下发 → 用户拉取，走通一整条链路。
func TestMailSendAndList(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138001", "mail-password")
	token := h.login("13800138001", "mail-password")

	code, body := h.sendMail(uid, "系统奖励", []map[string]int{{"item_id": 1001, "count": 5}})
	if code != http.StatusCreated {
		t.Fatalf("下发失败: code=%d body=%v", code, body)
	}

	body = h.listMails(token)
	if n, _ := body["count"].(float64); n != 1 {
		t.Fatalf("应当有 1 封邮件, got %v: %v", n, body)
	}
	mails, _ := body["mails"].([]any)
	m, _ := mails[0].(map[string]any)
	if m["title"] != "系统奖励" {
		t.Fatalf("标题不对: %v", m["title"])
	}
	if claimed, _ := m["claimed"].(bool); claimed {
		t.Fatal("刚下发的邮件不该是已领取状态")
	}
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("附件应当有 1 项: %v", m["items"])
	}
}

// TestMailSendToOfflineUser 下发不看用户在没在线——这正是邮件做成 Mgr 的原因。
//
// 这里的"离线"是真离线：注册完立刻把他的用户 actor 回收掉，再下发。
func TestMailSendToOfflineUser(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138002", "mail-password")

	if n := h.hub.OnlineUsers(); n != 0 {
		t.Fatalf("注册之后不该有用户 actor 在线, got %d", n)
	}
	if code, body := h.sendMail(uid, "离线也能收", nil); code != http.StatusCreated {
		t.Fatalf("给离线用户下发失败: code=%d body=%v", code, body)
	}
	if n := h.mails.count(uid); n != 1 {
		t.Fatalf("存储里应当有 1 封, got %d", n)
	}

	// 之后他登录，能看到
	token := h.login("13800138002", "mail-password")
	body := h.listMails(token)
	if n, _ := body["count"].(float64); n != 1 {
		t.Fatalf("登录后应当看到 1 封, got %v", n)
	}
}

// TestMailKeepLimit 到达上限后新邮件顶掉最老的一封。
//
// 需求写的是"最多保留 100 封，新增将把最老的一封顶走"。这条验三件事：
// 总数不超上限、顶掉的是最老那封、留下的是最新的那批。
func TestMailKeepLimit(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138003", "mail-password")
	token := h.login("13800138003", "mail-password")

	total := comm.MailKeepLimit + 5
	for i := 0; i < total; i++ {
		if code, body := h.sendMail(uid, fmt.Sprintf("第%d封", i), nil); code != http.StatusCreated {
			t.Fatalf("第 %d 封下发失败: code=%d body=%v", i, code, body)
		}
	}

	if n := h.mails.count(uid); n != comm.MailKeepLimit {
		t.Fatalf("存储里应当正好 %d 封, got %d", comm.MailKeepLimit, n)
	}

	body := h.listMails(token)
	mails, _ := body["mails"].([]any)
	if len(mails) != comm.MailKeepLimit {
		t.Fatalf("应当返回 %d 封, got %d", comm.MailKeepLimit, len(mails))
	}

	// 最前面的是最后发的那封
	first, _ := mails[0].(map[string]any)
	if want := fmt.Sprintf("第%d封", total-1); first["title"] != want {
		t.Errorf("最新的一封应当排在最前, want %s got %v", want, first["title"])
	}
	// 最老的 5 封应当已经被顶掉
	titles := map[string]bool{}
	for _, raw := range mails {
		m, _ := raw.(map[string]any)
		titles[fmt.Sprint(m["title"])] = true
	}
	for i := 0; i < 5; i++ {
		if titles[fmt.Sprintf("第%d封", i)] {
			t.Errorf("第%d封是最老的那批，应当已被顶掉", i)
		}
	}
}

// TestMailClaimOnce 附件只能领一次。
//
// 这是邮件模块最需要串行化的地方：领两次就是双倍发放。
func TestMailClaimOnce(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138004", "mail-password")
	token := h.login("13800138004", "mail-password")

	h.sendMail(uid, "带附件", []map[string]int{{"item_id": 2001, "count": 3}})
	body := h.listMails(token)
	mails, _ := body["mails"].([]any)
	mailID, _ := mails[0].(map[string]any)["mail_id"].(string)

	// 第一次：**同步**拿到权威结果，不是"已受理"
	code, body2 := h.claim(token, mailID)
	if code != http.StatusOK {
		t.Fatalf("第一次领取应当成功并直接给出结果: code=%d body=%v", code, body2)
	}
	items, _ := body2["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("应当领到 1 项道具: %v", body2)
	}

	// 第二次要被挡住，且状态码要区别于"邮件不存在"
	code, body2 = h.claim(token, mailID)
	if code != http.StatusConflict {
		t.Fatalf("重复领取应当 409, got %d body=%v", code, body2)
	}

	// 用户侧那份视图随后会被 mgr 推新
	h.waitClaimed(token, mailID)
}

// TestMailConcurrentClaim 并发领同一封邮件，只能成功一次。
//
// 判定在 MailMgr —— 权威状态在存储上，而存储只有它碰得到。按 UID 分片保证了
// 这个用户的所有邮件操作排在同一条事件循环上，"读—判断—写"因此不需要任何锁。
//
// 这也是"写必须直连权威侧"的理由：换成让用户侧的 mod 先判一遍再转发，
// 两个在线端各自的判断都会觉得自己可以领。
func TestMailConcurrentClaim(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138005", "mail-password")
	token := h.login("13800138005", "mail-password")

	h.sendMail(uid, "并发领取", []map[string]int{{"item_id": 3001, "count": 1}})
	body := h.listMails(token)
	mails, _ := body["mails"].([]any)
	mailID, _ := mails[0].(map[string]any)["mail_id"].(string)

	const attempts = 16
	codes := make([]int, attempts)
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			codes[i], _ = h.do(http.MethodPost, "/api/mails/claim", token,
				map[string]string{"mail_id": mailID})
		}(i)
	}
	wg.Wait()

	ok, conflict, other := 0, 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			other++
		}
	}
	// 恰好一次成功。判定全部落在 MailMgr 那一个分片上——同一个 UID 的所有邮件
	// 操作排在同一条事件循环里，"读—判断—写"天然串行，不需要任何锁。
	if ok != 1 || other != 0 {
		t.Fatalf("%d 次并发领取: 成功 %d 冲突 %d 其它 %d，应当是 1/%d/0",
			attempts, ok, conflict, other, attempts-1)
	}
}

// TestMailOpsAuth 下发接口必须有运维令牌，且不能用用户的 Bearer 冒充。
//
// 这个接口能凭空发放道具，是整个服务里唯一"不经过用户身份就能改用户数据"的口子。
func TestMailOpsAuth(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138006", "mail-password")
	userToken := h.login("13800138006", "mail-password")

	body := map[string]any{"uid": uid, "title": "越权", "content": "x", "items": nil}
	for _, c := range []struct {
		name, token string
	}{
		{"无令牌", ""},
		{"用户的 Bearer", userToken},
		{"乱猜的令牌", "wrong-token"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if code, _ := h.do(http.MethodPost, "/api/mail/send", c.token, body); code != http.StatusUnauthorized {
				t.Fatalf("应当 401, got %d", code)
			}
		})
	}
	if n := h.mails.count(uid); n != 0 {
		t.Fatalf("越权尝试不该发出任何邮件, got %d", n)
	}
}

// TestMailSendValidation 运维填错参数要被挡住。
func TestMailSendValidation(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138007", "mail-password")

	for _, c := range []struct {
		name     string
		body     map[string]any
		wantCode int
	}{
		{"正常", map[string]any{"uid": uid, "title": "t", "content": "c", "items": nil}, http.StatusCreated},
		{"UID 不是手机号", map[string]any{"uid": "abc", "title": "t", "content": "c", "items": nil}, http.StatusBadRequest},
		{"标题为空", map[string]any{"uid": uid, "title": "", "content": "c", "items": nil}, http.StatusBadRequest},
		{"道具数量为 0", map[string]any{"uid": uid, "title": "t", "content": "c",
			"items": []map[string]int{{"item_id": 1, "count": 0}}}, http.StatusBadRequest},
		// 负数最危险：入包时会变成扣物品，把发奖接口变成扣道具接口
		{"道具数量为负", map[string]any{"uid": uid, "title": "t", "content": "c",
			"items": []map[string]int{{"item_id": 1, "count": -5}}}, http.StatusBadRequest},
		{"道具 ID 非法", map[string]any{"uid": uid, "title": "t", "content": "c",
			"items": []map[string]int{{"item_id": 0, "count": 1}}}, http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.do(http.MethodPost, "/api/mail/send", opsToken, c.body)
			if code != c.wantCode {
				t.Fatalf("code=%d want=%d body=%v", code, c.wantCode, body)
			}
		})
	}
}

// TestMailIsolatedPerUser 一个用户看不到另一个用户的邮件。
func TestMailIsolatedPerUser(t *testing.T) {
	h := newHarness(t)
	a := h.register("13800138008", "mail-password")
	h.register("13800138009", "mail-password")
	bToken := h.login("13800138009", "mail-password")

	h.sendMail(a, "只给 A 的", nil)

	body := h.listMails(bToken)
	if n, _ := body["count"].(float64); n != 0 {
		t.Fatalf("B 不该看到 A 的邮件, got %v: %v", n, body)
	}
}

// TestMailboxReadIsLocal 信箱就绪之后，用户侧的读**完全不回 MailMgr**。
//
// 这是给用户加一个 MailboxMod 的全部理由。用户量大时一个 mgr 分片服务成千上万
// 人，每次拉邮件都回 mgr 要一遍，等于把所有人的读请求排到那几条事件循环上。
// 数据由 mgr 在登录时推一次，之后读全部落在用户自己那条协程里。
//
// 用存储的 Load 次数来验：mgr 是唯一碰存储的一方，Load 次数不涨就说明读没出去。
func TestMailboxReadIsLocal(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138010", "mail-password")
	h.sendMail(uid, "先发一封", nil)
	token := h.login("13800138010", "mail-password")

	h.listMails(token) // 等就绪，这一步会触发 mgr 读存储
	base := h.mails.loadCount()

	for i := 0; i < 20; i++ {
		if code, body := h.do(http.MethodGet, "/api/mails", token, nil); code != http.StatusOK {
			t.Fatalf("第 %d 次拉取失败: code=%d body=%v", i, code, body)
		}
	}

	if n := h.mails.loadCount(); n != base {
		t.Errorf("20 次拉取让存储被读了 %d 次（就绪前 %d 次）——"+
			"读应当全部落在用户自己的 actor 上，一次都不该回 MailMgr", n-base, base)
	}
}

// TestMailboxNotReadyIsNotEmpty 数据还在路上时，不能伪装成"你没有邮件"。
//
// 这两种状态在客户端要显示成不同的东西：一个是转圈，一个是空信箱。
// 服务端把它们混成同一个 200 + 空列表，客户端就再也分不开了。
func TestMailboxNotReadyIsNotEmpty(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138011", "mail-password")
	h.sendMail(uid, "有邮件的", nil)

	// 直接问一个从没上线过的用户——他的 MailboxMod 是现建的，数据必然还没到
	code, body := h.do(http.MethodGet, "/api/mails",
		h.login("13800138011", "mail-password"), nil)

	// 要么已经就绪（推得快），要么明确是"加载中"，绝不能是 200 + 空列表
	if code == http.StatusOK {
		if n, _ := body["count"].(float64); n == 0 {
			t.Fatalf("就绪了却说 0 封，可这个用户明明有一封: %v", body)
		}
		return
	}
	if code != http.StatusAccepted {
		t.Fatalf("未就绪时应当返回 202, got %d body=%v", code, body)
	}
	if ready, _ := body["ready"].(bool); ready {
		t.Fatalf("202 却说 ready=true: %v", body)
	}
}

// TestMailClaimIsAuthoritative 领取的返回值必须是**权威结果**，不能是本地旧快照。
//
// 这条钉的是一个真实踩过的 bug。早先的写法是 rut 调用户侧的 MailboxMod，
// 由它本地标记"处理中"、异步通知 MailMgr、然后**立刻返回自己手上那份还没改的
// 数据**。症状很隐蔽：HTTP 回 2xx、返回体看着正常，可 mgr 那边的读改写还没跑，
// 甚至可能根本改不成（邮件已被顶掉、附件已领过）。客户端据此以为领取成功了。
//
// 根子是权威状态在存储上、而存储只有 mgr 碰得到。所以写一律 rut 直连 mgr。
// 这里用一封**用户侧视图里明明存在、权威侧却已经领过**的邮件来验：
// 如果实现退回"读本地再返回"，它会回成功；直连权威侧才会正确地报 409。
func TestMailClaimIsAuthoritative(t *testing.T) {
	h := newHarness(t)
	uid := h.register("13800138012", "mail-password")
	token := h.login("13800138012", "mail-password")

	h.sendMail(uid, "只能领一次", []map[string]int{{"item_id": 4001, "count": 2}})
	box := h.listMails(token)
	mails, _ := box["mails"].([]any)
	mailID, _ := mails[0].(map[string]any)["mail_id"].(string)

	// 先领掉。此刻权威侧已是 claimed=true。
	if code, body := h.claim(token, mailID); code != http.StatusOK {
		t.Fatalf("第一次领取应当成功: code=%d body=%v", code, body)
	}

	// 把用户侧的视图强行退回"还没领"的旧状态——模拟 mgr 的推送还没到、
	// 或者干脆丢了。本地看起来这封仍然可领。
	stale := make([]comm.MailSnap, 0, 1)
	for _, m := range h.mails.snapshot(uid) {
		m.Claimed = false
		stale = append(stale, m)
	}
	h.hub.MailboxPush(0, uid, stale)

	// 确认视图确实"看起来可领"了，否则这条测试什么也没验到
	box = h.listMails(token)
	mails, _ = box["mails"].([]any)
	if claimed, _ := mails[0].(map[string]any)["claimed"].(bool); claimed {
		t.Fatal("没能把用户侧视图退回旧状态，这条测试失去意义")
	}

	// 再领一次：本地视图说可以，权威侧说不行——必须听权威侧的
	code, body := h.claim(token, mailID)
	if code != http.StatusConflict {
		t.Fatalf("本地视图过期时仍必须由权威侧判定，应当 409, got %d body=%v", code, body)
	}
}
