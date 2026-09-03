# noteserver

基于 actor 框架的笔记服务示例：手机号注册、登录、上传笔记、获取笔记。

它是一个**独立的 Go 模块**，有自己的 `go.mod`，通过 `replace` 指回上两级的 actor
框架。这样框架本身不必为了一个示例背上 gin 和 ORM 的依赖。

技术栈：[Gin](https://github.com/gin-gonic/gin) + actor 框架 + [Norm ORM](https://github.com/norm)（MySQL + Redis）。
目录结构参考 `roleSvr`。

## 起服务

```bash
mysql -uroot -p < schema.sql          # 只建库；表由 Norm 的 AutoMigrate 建
go run .                              # 默认读 ./data 下的两份配置
go run . -data /path/to/data          # 或指定配置目录
```

两份配置各管一摊，分开是有意的——换数据库地址不该碰服务器配置，反之亦然：

| 文件 | 管什么 | 谁读它 |
|---|---|---|
| `data/server.json` | 监听地址、端口、gin 模式 | `src/config` |
| `data/orm.json` | MySQL / Redis 连接、刷盘参数 | `orm.InitPool` |

环境变量优先于 `server.json`，所以线上换端口不必改文件：

```bash
SERVER_PORT=18080 go run .
```

## 项目规范

这一节是**约束**，不是建议。新加代码前先看它；`layering_test.go` 会把其中的依赖
规则和文件命名规则当测试跑，违反了直接红。

### 目录与依赖

```
main.go              只做装配：读配置 → 起连接池 → 建 Hub → 挂路由 → Run
data/                两份配置
templates/           门面的生成模板
src/
  comm/              全项目可用的数据源：常量 + **Snap 值类型（文件一律 **_comm.go）
  contract/          只有接口，以及它们返回的错误哨兵
  databases/         需要存档的对象
  middleware/        gin 中间件
  mods/              逻辑模块，按功能分文件夹
    auth/  note/
  router/            路由，按功能分文件夹
    auth/  note/  health/
  service/           最上层终端：Hub + 生成的门面（**_export.go）
  ── 以下三个是规范七个文件夹之外的补充（见第 8 条），同样受依赖约束管 ──
  bases/             HTTP 基座：gin 引擎、应答格式、actor 错误 → HTTP 语义
  logs/              全项目唯一的日志出口（底层 GCore）
  security/          bcrypt、令牌、手机号与密码校验
  config/            服务器自身配置
```

依赖只能自上而下。**这张表就是 `layering_test.go` 里的那张表**，改代码要连它一起改：

| 包 | 允许引入的内部包 |
|---|---|
| `comm` | **无** —— 依赖图的底层。连 `logs` 都不引：里面只有数据，没有会失败的逻辑 |
| `contract` | `comm` |
| `databases` | `comm` `contract` `logs` |
| `bases` / `security` | `comm` `logs` |
| `logs` | **无** —— 它只包着 GCore，谁都能引 |
| `config` | 无 |
| `middleware` | `bases` `comm` `contract` `logs` |
| `mods/*` | `comm` `contract` `logs` —— **绝不能引 service** |
| `router/*` | `bases` `comm` `contract` `security` `middleware` `logs` |
| `service` | 全部 |

### 文件命名总表

| 位置 | 文件名 | 里面定义什么 |
|---|---|---|
| `comm/` | `**_comm.go` | `**Snap` 值类型、常量。**本包所有文件都要这个后缀** |
| `mods/<功能>/` | `**_mod.go` | `**Mod` —— 挂在用户身上，组合 `actor.ModObj` |
| `mods/<功能>/` | `**_mgr.go` | `**Mgr` —— 挂在服务器上，组合 `actor.ModObj` |
| `mods/<功能>/` | `**_imp.go` | `**Imp` —— 细分逻辑，**不**组合 `ModObj`，由 Mod/Mgr 持有 |
| `mods/<功能>/` | `**_type.go` | 模块私有的数据类型 |
| `router/<功能>/` | `**_rut.go` | `**Rut` —— 组合 `web.Router` |
| `router/<功能>/` | `**_type.go` | `**Request` / `**Response` |
| `service/` | `**_export.go` | **生成物，严禁手改**；名字 = 模块文件名剥掉 `_mod`/`_mgr` 加 `_export` |

一个功能文件夹**有且只有一个** `**_mod.go` 或 `**_mgr.go`（`TestModFileNaming` 会拦）；
每个路由文件夹**至少一个** `**_rut.go`（`TestRouterFileNaming` 会拦）。

### 1. comm —— 全项目的数据源

**1.1 文件一律 `**_comm.go`**，常量文件也不例外（`consts_comm.go`）。

**1.2 常量按功能归位。** 判断标准只有一条：

> **这个常量是不是被多个功能模块用到？**
> 是 → 放 `consts_comm.go`；否 → 放那个功能自己的 `**_comm.go`。

邮件的常量放 `mail_comm.go`、笔记的放 `note_comm.go`、账号与会话的放
`account_comm.go`。**默认答案是"否"**——绝大多数常量只服务一个功能，
往 `consts_comm.go` 里堆是省事，代价是那个文件很快变成谁都要读、谁都读不懂的
一大坨，而且删功能时没人敢动它。

现在留在 `consts_comm.go` 里的只有三个，每个都能说出"哪几个功能在用"：

| 常量 | 谁在用 |
|---|---|
| `MaxBodyBytes` | 所有路由（共用同一份 `web.Router` 选项） |
| `UserIdleTimeout` / `JanitorInterval` | 所有 `**_mod.go`（用户 actor 的生命周期） |

**1.3 内容命名**：常量**标识符**无命名要求；struct **必须以 Snap 结尾**。
注意 1.1 与这条别混——"无命名要求"说的是常量叫什么名字随意，不是说文件名可以随意。

Snap 是个标记，含义是"这个值可以跨模块传递"。反过来说，不带 Snap 的类型就不该
出现在模块与模块之间——那类型属于某个模块自己，别人不该认识它。**AMod 需要
BMod 的数据时，BMod 只能返回 `**Snap`。**

**本包不引入任何其他内部包。** 一旦它开始 import contract 或 mods，分层就塌了。

### 2. contract —— 只有接口

只放接口，以及与接口直接相关的东西（这里是它们返回的错误哨兵）。**不放普通对象**
——跨模块传的值一律去 comm 定义成 `**Snap`。只能引入 `comm`。

### 3. databases —— 需要存档的对象

所有需要存档的对象定义在这里，**不论它最终存到哪**（本项目里 account/note 落
MySQL+Redis，session 只落 Redis，都在这个文件夹）。只能引入 `comm` 和 `contract`。

### 4. middleware —— 中间件

gin 中间件放这里。

### 5. mods —— 逻辑模块

**按功能分文件夹**，一个功能一个文件夹（`mods/auth/`、`mods/note/`）。

**一个功能文件夹里 `**_mod.go` 与 `**_mgr.go` 各最多一个**，但两者可以并存。
命名规则：

| 后缀 | 含义 | 判断标准 | 谁加载 |
|---|---|---|---|
| `**_mod.go` → `**Mod` | 挂在**用户**身上 | 没有用户时这个功能没事可做 | 用户登入成功后 |
| `**_mgr.go` → `**Mgr` | 挂在**服务器**上 | 没有用户时也需要正常运行 | 服务器启动时 |

登入验证、邮件下发都属于后者——用户还没登进来，谁给他挂模块？

**一个功能同时需要两者是常见的**，邮件就是：`MailMgr` 管存储与下发（用户离线也要
能收），`MailboxMod` 管这个用户在线期间的信箱视图（读不出他自己那条协程）。
本项目里 `auth` 只有 Mgr，`note` 只有 Mod，`mail` 两者都有。

> 有个约束要知道：生成物文件名是"剥掉 `_mod`/`_mgr` 再加 `_export`"，所以同一个
> 文件夹里 `xxx_mod.go` 和 `xxx_mgr.go` 会**撞成同一个生成物**。邮件因此把用户侧
> 命名为 `mailbox_mod.go` / `MailboxMod`——顺带这个名字也更准，它就是用户的信箱。

入口类型必须组合 `actor.ModObj[*自己]`，构造函数返回 `actor.IModule`
（不返回具体类型，是为了不给调用方"绕过 actor 直接调方法"的机会）。

**功能复杂时用 `**_imp.go` 细分**：在同一个文件夹里定义 `**Imp` 对象，由 `**Mod` /
`**Mgr` 持有。`**Imp` **不再组合 `actor.ModObj`**——跨 goroutine 的入口只该有一个。
再开一个 `_mod.go` 是不允许的，`TestModFileNaming` 会拦。

**独属于本模块的数据类型放 `**_type.go`。** 一旦发现别的模块也要用它，就去 comm
建 `**_comm.go` 定义 `**Snap`，对外一律用 Snap 顶替。本项目两个模块都没有
`**_type.go`：它们的数据形状和 `comm.NoteSnap` / `comm.AccountSnap` 完全一致，
再造一个模块私有类型只是抄字段。

### 5.1 mods 之间的调用，不可以使用反参

**规则：模块与模块之间的调用一律不带返回值，只能是单向通知。没有例外。**

这是整套设计里最容易写错、错了又最难查的一条。

**为什么。** 框架的事件循环是**单消费者**：一个模块方法只要在等另一个 actor 的
返回值，它自己的队列就停着不动。后果有两层——

- 轻的是**吞吐**：即使不死锁，调用方在等待期间什么也干不了，它服务的所有请求
  一起排队。用户量大时这一点比死锁更常见。
- 重的是**死锁**：两个模块互相等就锁死了。而框架的环检测只覆盖同步调用，
  真写成同步之后，能救你的只有 3 秒超时——那时你看到的是一堆莫名其妙的超时，
  不是一条指向根因的错误。

**界线在"谁调它"，不在方法本身**：

| 调用方 | 能不能要返回值 | 为什么 |
|---|---|---|
| rut → mod/mgr | **能** | rut 跑在 gin 的请求协程上，不会被任何模块调用；阻塞它只影响这一个 HTTP 请求 |
| mgr ↔ mod | **不能** | 两边都是事件循环，等待会让调用方停摆，互等就是死锁 |

所以 `MailboxMod.List` 有返回值是对的（rut 调），`mailPusher.MailboxPush` 没有是
必须的（模块调）。

**这条是硬约束，由 `TestModsInterfacesAreNotifyOnly` 守着**：mods 包里声明的接口
一律不带返回值——那些接口就是模块之间所有调用的出口，卡住它们等于把规则变成
机器能验的东西。

不留"确有必要时可以破例"的口子，理由是这类破例判断不了：写的时候你只看得见
"A 调 B、B 不会回调 A"，看不见半年后有人给 B 加了一条通往 A 的边。而症状是偶发
超时和吞吐塌陷，最难归因。

**觉得需要反参时，说明该换的是设计而不是规则。** 常见的两种改法：

- **要数据** → 反过来推：让持有数据的一方在数据就绪时通知你，而不是你去问它。
  邮件就是这么做的——`MailMgr` 推给 `MailboxMod`，而不是 mod 去 mgr 拉。
- **要结果** → 别经手：让真正需要结果的人（rut）直连权威方。rut 跑在请求协程上，
  阻塞它只影响一个请求。邮件的领取就是这么改的（见 5.2），改之前那版正是
  "mod 转发再返回本地数据"，返回的根本不是操作结果。

### 5.2 mod 只读，mgr 读写：写必须直连权威侧

邮件的分工：

```
写 rut --MailSend / MailClaim--> mgr    权威侧改存储，带返回值同步给结果
读 rut --MailboxList----------> mod    本地读，不出用户那条协程
同步   mgr --MailboxPush------> mod    改完之后单向通知，不等回执
上线   Hub --MailFetch--------> mgr    单向通知，让它推一次
```

**这里踩过一个坑，值得写下来。** 早先把写做成"rut → mod → 异步通知 mgr"：
mod 本地标记"处理中"、把请求转给 mgr，然后**立刻返回自己手上那份还没改的数据**。

症状极隐蔽：HTTP 回 2xx、返回体看着正常，可 mgr 那边的读改写还没跑，甚至可能
根本改不成（邮件已被顶掉、附件已领过）。**rut 拿到的根本不是操作结果**，
客户端却据此以为成功了。

根子是：**权威状态在存储上，而存储只有 mgr 碰得到。** "改完了吗、改成什么样"
这类问题只有 mgr 答得准，让 mod 去答它只能猜。所以写一律直连权威侧并带返回值。

改过之后 `MailboxMod` **一条对外调用都没有**了：它只接收推送、只回答读请求，
永远不会停下来等任何人。这个性质比"小心别写成同步"可靠得多——连写错的地方都没有。
`TestMailClaimIsAuthoritative` 把这条钉住：故意让用户侧视图退回"还没领"的旧状态，
再领一次必须由权威侧判成 409。

剩下的代价只有一个，而且是读缓存的固有代价：**用户侧视图会比权威侧晚一点点**。
领取响应本身是权威的，但紧接着再拉一次列表，理论上有极小的窗口仍看到
`claimed=false`。真实游戏里客户端等的是服务端推送而不是轮询。

生成门面的 `user_export.tmpl` 也按这条分流：有返回值走 `AcquireUser`（请求，
actor 不在就建），无返回值走 `TryUser`（通知，用户不在线就丢弃、**不创建 actor**）。
用 `AcquireUser` 去推会给一个刚被回收的用户重新拉起协程，把"在线用户数"悄悄变成
"登录过的用户数"。

### 6. router —— 路由

**按功能分文件夹**，每个文件夹至少一个 `**_rut.go`，其中定义 `**Rut` 对象并组合
`web.Router[*自己]`（`TestRouterFileNaming` 会拦缺失）。

请求与响应类型单独放 `**_type.go`，命名 `**Request` / `**Response`。请求类型内嵌
`web.POST` 等标记声明 HTTP 动词，可选 `path` tag 覆盖路径：

```go
// note_type.go
type UploadRequest struct {
    web.POST `path:"/notes"`
    Content  string `json:"content"`
}

// note_rut.go
type NoteRut struct {
    web.Router[*NoteRut]
    hub service
}
func (that *NoteRut) Upload(req *UploadRequest, ctx *gin.Context) { ... }
```

### 7. service —— 最上层终端

可以引入所有包。至少有一个 `Hub`：启动时加载 `**_mgr.go` 的模块，用户登入成功后
给他加载 `**_mod.go` 的模块。

`**_export.go` 是**框架生成的，严禁手改**——改了下次 `go generate` 就没了，
`TestFacadesUpToDate` 也会红。要改行为去改模块方法上的 `export:` 标记。

文件名由模块文件名推出：**剥掉末尾的 `_mod` / `_mgr`，再加 `_export`**。

```
mods/auth/auth_mgr.go  →  service/auth_export.go
mods/note/note_mod.go  →  service/note_export.go
```

生成物里不带 mod/mgr 是有意的：那个后缀描述的是"模块挂在谁身上"，而门面没有这个
区别——它们都是挂在 Hub 上的普通方法。文件名里留着只会让人以为那是两类东西。

**但不是每个模块都会有生成物。** 一个模块完全可以只有内部方法、一个 `export:`
标记都不带，那时它不产生任何 `**_export.go`，这是**合法设计**而不是遗漏：

```
mods/xxx/xxx_mod.go  有 export: 标记  →  service/xxx_export.go
mods/yyy/yyy_mod.go  没有任何标记      →  不产生文件（合法）
```

`gercmd verify` 把这种情况单独报成 `noexport` 并放行，`-strict` 也不升级它 ——
`-strict` 只针对"漏写了 `//go:generate` 指令"。反过来，**标记删了而生成物没删**
会被报成 `leftover` 失败：那份残留仍在参与编译、仍对外暴露着一个已经不该导出的
方法。删标记时记得把生成物一起删掉，或者直接跑一次 `go generate` 让它自己消失。

**mod 与 mgr 之间不可以直接调用公有函数，必须走 `**_export.go` 里的导出函数。**
理由是不能假定两个模块处于同一条协程——直接调就是在别人的事件循环之外改他的状态，
actor 的全部保证瞬间失效。

> 做法上有个坑：`mods` **不能 import `service`**（会与 `service → mods` 成环，
> 分层测试也会拦）。所以模块要调别人时，在自己包里声明一个只含所需方法的小接口，
> 由 Hub 满足它、在构造时注入。本项目暂时没有跨模块调用，真要加时按这个来。

### 7.1 日志一律走 logs 包

**不要直接用标准库 `log`/`fmt`，也不要直接用 GCore。** 全项目只有
`src/logs` 一个出口，底层是 GCore 的 logger（logrus + 按天切割落盘）。

包一层的理由：GCore 的包名是 `utils`，散到几十个文件里，import 行全是
`utils "github.com/yinchnag/GCore"`，读代码的人看不出那是日志库；换实现时也只
需要改一个文件。

**等级不是风格问题，选错有实际代价**：GCore 的 `Errorf` 会往日志里附一整份
`debug.Stack()`，配置里 `assert` 打开时还会 panic。把预期内的失败写成 `Errorf`，
错误日志会被几十行栈淹没，真正的 bug 反而找不着。

| 等级 | 用在哪 |
|---|---|
| `Debugf` | 排障细节，线上通常关掉 |
| `Infof` | 生命周期事件：启动、监听、路由表、退出 |
| `Warnf` | **预期内的失败**——存储抖动、会话读不到、调用超时、用户输错。不带栈 |
| `Errorf` | **说明有 bug**——不该发生却发生了。带完整调用栈 |
| `Fatalf` | 启动期无法继续。**只该出现在 main 里** |

判据一句话：**这条日志出现时，需不需要有人去改代码？** 需要就 `Errorf`，
不需要就 `Warnf`。所以"调用超时"是 Warnf（下游慢而已），"跨 actor 调用环"是
Errorf（编排写错了，得改代码）。

> `src/logs/caller.go` 里挂了一个 hook 修正 `entry.Caller`。不修的话，logrus 会把
> 包装层当成调用方，所有日志的位置都显示成 `logs.go` 的某一行——日志失去定位
> 价值，比不包装更糟。那个 hook 还必须排在 GCore 写文件的 hook **前面**，
> 否则控制台是对的、文件里是错的，两份日志对不上最难排查。

日志落在 `<工作目录>/logs/`，按天切割保留 7 天，已在 `.gitignore` 里。

### 8. 别的文件夹

不建议再建新文件夹，但不拦着。真建了就在 `layering_test.go` 的表里登记它的依赖
约束——否则那个测试会因为"包不在分层表里"直接红。`bases` / `security` / `config`
就是这么来的。

### 加一个功能模块时会撞上的

下面这些是**照着规范真加一个功能（邮件）时逐个撞出来的**，不是设想。规范条文本身
看着都很清楚，坑全在条文之间的缝里。按撞上的顺序列：

| 现象 | 根因 | 现在的做法 | 谁在守 |
|---|---|---|---|
| 加第二个分片 Mgr 时，`shard_export.tmpl` 完全不能用 | 模板里写死 `that.AuthFor(uid)`，Hub 里也只有一个写死的 `auth` 数组 | Hub 改成按模块名索引的分片注册表，模板写 `that.ShardFor("{{.ModType}}", uid)` | 加模块不必碰模板即为通过 |
| 模块的业务错误没地方放 | 它不是存储接口返回的，不属于 `contract`；放 `mods` 则 router 要反向依赖模块包 | 放 `comm`——所有人都能引，依赖图不变 | `TestLayering` |
| 一个功能的接口分属两种身份，挂不到一个路由对象上 | `Router.Init` 会把宿主**所有**公有方法都扫成路由 | 拆成两个 Rut 对象（`MailRut` 运维 / `MailUserRut` 用户） | — |
| 同一文件夹的 `xxx_mgr.go` 与 `xxx_mod.go` 生成物撞名 | 文件名规则是"剥掉 `_mod`/`_mgr` 再加 `_export`" | 用户侧换个名字（`mailbox_mod.go`）；顺带那名字也更准 | 撞名时 `verify` 会报 stale |
| 一个模块同时有通知型和请求型方法，一份模板装不下 | 一个模块文件只能指定一份模板 | **一份模板按有无返回值分流**：有返回走 `AcquireUser`，无返回走 `TryUser` | 模板里的 `#` 守卫 |
| 推送不能用 `AcquireUser` | 那会给已被回收的用户重新拉起协程，把"在线用户数"变成"登录过的用户数" | 新增 `Hub.TryUser`：不在线返回 nil，直接丢弃 | — |
| **写经 mod 转发，返回的不是操作结果** | 权威状态在存储上，而存储只有 mgr 碰得到；mod 只能返回自己手上那份没改的旧数据 | 写一律 rut 直连 mgr 并带返回值（见 5.2） | `TestMailClaimIsAuthoritative` |

最后一条最值得记：它的症状是 **HTTP 回 2xx、返回体看着完全正常**，而权威侧可能
根本没改成。这类"看起来成功了"的错，测试不专门盯着写就发现不了。

工具侧撞出来的几件（已在 `cmd/gercmd/README.md` 里）：`check` 原先无条件补 `Mod`
后缀所以认不出 `AuthMgr`；`verify` 曾经让"没有 export 标记的模块"给残留生成物打
掩护，`-strict` 又把两种含义相反的 skipped 混为一谈。

### 与参考项目 roleSvr 的一点不同

**存储藏在 `contract` 的接口后面。** `roleSvr` 的 service 直接调 databases，那是业务
服务器的写法；这里多一层接口，是为了让整套 actor 编排能在**没有 MySQL/Redis** 的
机器上被完整测到（见"测试"一节）。示例跑不起测试就失去了示例的意义。

## 自动注册路由

路由是自动生成的，全项目没有一处 `group.POST("/login", ...)`。做法取自 `roleSvr`
的 `bases/router.go`：路由结构体内嵌 `Router[*自己]`，`Init` 反射扫出它的公有方法，
从**请求结构体内嵌的动词标记**推出 HTTP 方法与路径。

这套东西已经从本项目抽出去，放在仓库根的 [`web/`](../../web/) —— 与 actor 平级的
独立模块，以后别的 HTTP 服务直接 `require` 它即可。完整说明见 `web/README.md`，
下面只讲本项目怎么用它。

```go
type Auth struct {
    web.Router[*Auth]     // 类型参数必须是自己
    hub service
}

type LoginRequest struct {
    web.POST              // 动词；没写 path tag 就按方法名推出 /login
    Phone    string `json:"phone"`
    Password string `json:"password"`
}

// 扫到这个方法 → 注册 POST /api/login，req 已经绑定好
func (that *Auth) Login(req *LoginRequest, ctx *gin.Context) { ... }
```

加一个接口 = 加一个方法 + 一个请求类型。`main` 只决定挂在哪个分组、套哪些中间件。

`web` 只管"怎么把方法扫成路由"，请求体上限和错误应答形状这两件**业务决定**由本项目
通过 `bases.RouterOpts()` 喂进去 —— 于是 `web` 内部的绑定失败与本项目 handler 手写的
错误走同一个出口，客户端看到的形状一致。

它和 actor 框架的 `ModObj[T]` 是同一种 CRTP 思路，但**没有复用它**——`ModObj` 解决
的是"跨 goroutine 按方法名调用模块"，这里解决的是"按方法签名生成 HTTP 路由"，
方法表形状、校验规则、失败时机都不一样。硬凑成一个会把 HTTP 层的需求倒灌进框架。

### 相对 roleSvr 的三处改动（现已在 web 模块里）

**路径可由 tag 指定。** `roleSvr` 只按方法名小写推路径，那样 GET/POST 共用一个路径
（本项目的 `/notes`）就表达不出来：

```go
type ListRequest struct{ web.GET `path:"/notes"` }
type UploadRequest struct {
    web.POST `path:"/notes"`
    Content  string `json:"content"`
}
```

**请求体默认自动绑定**，不必每个请求类型手写 `FromWithContext`。POST/PUT 走
`BindJSON`（带请求体上限与 `DisallowUnknownFields`），GET/DELETE 按 `form` tag 绑
query，只有动词标记、没有字段的请求类型则完全跳过绑定。需要完全接管时实现
`web.IRequest` 即可，那条口子仍然留着——但它返回 `bool`，解析失败会拦住业务方法，
`roleSvr` 的同名接口没有返回值，失败时业务方法照样会拿着半填充的参数被调用。

**签名不合规当场 panic**，不静默降级。`roleSvr` 在参数不是 struct 时会悄悄退化成
GET 路由，那种错误要等线上打不通接口才发现。这里参数个数、`*gin.Context` 的位置、
返回值、请求类型缺动词标记，任何一条不对都在 `Init` 就炸。

### 一个检测不到的误用

类型参数写错编译期发现不了，`Init` 只挡得住一半：

- **挡得住**：`Router[*T]` 里 `T` 根本没有内嵌 `Router[*T]`（指向了一个普通结构体）。
  偏移查找失败，panic。
- **挡不住**：`T` 指向了另一个**也正确内嵌着自己**的路由，比如 `Note` 里写成
  `Router[*Auth]`。`Auth` 确实有 `Router[*Auth]` 字段，偏移查找会"成功"，
  恢复出一个指向 `Note` 内存却被当成 `*Auth` 的指针。

后一种靠两层兜底，测试 `TestWrongTypeParamThatLooksValid` 把它们钉住了：启动日志里
`Routes()` 打的是 `Auth.*` 而不是 `Note.*`，一眼能看出来；真装配时 `Auth` 自己也要
注册，gin 会因为同路径重复注册直接 panic。

### 反射的开销

反射只出现在两处：**启动时**扫方法建路由表（一次性，不在运行时尺度上），
**每请求时**一次 `reflect.New` 造请求对象加一次 `reflect.Value.Call` 调方法。

把同一个 handler 挂在同一个引擎上、一条自动注册一条手写注册做对照
（`web/router_bench_test.go`，交替执行 6 轮）：

| | 自动注册 | 手写注册 | 差值 |
|---|---|---|---|
| ServeHTTP（无网络） | 2673~2843 ns | 2488~2611 ns | **+205 ns，+8%，+1 alloc** |
| 完整 HTTP（含 TCP） | 7018~7548 ns | 7006~7786 ns | 测不出来（四轮两胜两负） |
| 真实服务（MySQL+Redis） | ~70 µs/请求 | — | +0.3% |

205 ns 是稳定可复现的——六轮里自动每一轮都慢。但它只在摘掉网络之后才看得见：
带上 TCP 就落进 ±500 ns 的噪声里，再垫上 Redis 的同步写就彻底无关紧要。

两处里真正花钱的是 `Call`（~150 ns）而不是 `New`（~19 ns），差八倍，真要优化该盯
前者。实在需要的话，热路径可以手写显式注册——`Init` 收的是 `gin.IRoutes`，
两种方式能在同一个引擎上共存，上面那个对照基准本身就是这么搭的。

启动日志会把注册结果全打出来——路由不再写在代码里，这就成了唯一能一眼看全
"到底开了哪些接口"的地方：

```
路由 GET    /healthz             → Health.Status
路由 POST   /api/login           → Auth.Login
路由 POST   /api/register        → Auth.Register
路由 GET    /api/notes           → Note.List
路由 POST   /api/notes           → Note.Upload
```

## 模块与生成的门面

actor 模块放在 `src/mods/<功能>/`，遵循仓库的模块规范：struct 内嵌
`actor.ModObj[*自己]`；构造函数返回 `actor.IModule`；要对外暴露的方法带
`export:` 标记。`gercmd check` 能验：

```bash
go -C ../.. run ./cmd/gercmd check auth cmd/noteserver/src   # 找到 AuthMgr，3/3 通过
go -C ../.. run ./cmd/gercmd check note cmd/noteserver/src   # 找到 NoteMod，3/3 通过
```

`check auth` 分不清你指 `AuthMod` 还是 `AuthMgr`——名字里没这个信息，所以它把候选
都试一遍，哪个的 struct 真存在就检查哪个。约定不同的项目可以用 `-type-suffix` /
`-ctor-prefix` 改，gercmd 不写死这些。

路由**不直接调 `ModInvoke`**。方法上的标记会被生成成 `Hub` 上的门面：

```go
//	export: NoteAdd
func (that *NoteMod) Add(content string) (comm.NoteSnap, error)
```

↓ `go generate ./src/mods/`

```go
// src/service/note_export.go — Code generated; DO NOT EDIT.
func (that *Hub) NoteAdd(gid uint64, uid string, content string) (comm.NoteSnap, error) {
	loader := that.AcquireUser(uid)
	defer that.ReleaseUser(uid)
	out, err := loader.ModInvokeFrom(gid, "NoteMod", "Add", content)
	return unwrap[comm.NoteSnap](out, err, "NoteMod.Add")
}
```

于是路由那边是 `that.hub.NoteAdd(gid, uid, content)` —— 一个字符串字面量都没有，
改了模块方法名，路由**编译就红**，不必等运行期的 `module not found`。

### 为什么要自己写模板

`gercmd` 的内置模板是为 GameSvr 那种布局写的（宿主唯一、`that.GetModloader()`、
错误 `println` 掉）。noteserver 三处对不上，所以 `templates/` 下自带两份。

**选哪个，取决于"这个模块的 actor 怎么找到"，而不是模块叫什么名字。**

| 模板 | 什么时候用 | 门面里长什么样 |
|---|---|---|
| `user_export.tmpl` | **每用户一个 actor** 的模块，也就是所有 `**_mod.go` | 多一个 `uid` 参数用来选 actor；`AcquireUser` + `defer ReleaseUser` 由模板负责 |
| `shard_export.tmpl` | 挂在**按业务键分片**的 actor 上的模块 | loader 用 `that.AuthFor(uid)` 取，uid 就是模块方法的第一个参数 |

`**_mod.go` 一定走 `user_export.tmpl` —— 每用户 actor 的取用/归还是硬性的。
`**_mgr.go` 则要看 Hub 怎么持有它：本项目的 `AuthMgr` 是按手机号分片的，所以走
`shard_export.tmpl`；**如果以后加一个单例 Mgr**（比如全局只有一个的邮件模块），
那两份模板都不合适——`AuthFor(uid)` 拿不到它——需要再写一份取 loader 的方式不同的
模板。模板的差别只在"怎么拿到 loader"这一行，照着现有的改很快。

对应关系写在各自模块的 `//go:generate` 里，那也是 `gercmd verify` 读的地方：

```go
// mods/auth/auth_mgr.go
//go:generate ... gen -tmpl cmd/noteserver/templates/shard_export.tmpl -recv Hub ...

// mods/note/note_mod.go
//go:generate ... gen -tmpl cmd/noteserver/templates/user_export.tmpl -recv Hub ...
```

`user_export.tmpl` 把取用与归还收进门面，是这次改造最实在的收获：它们原先写在每个
handler 里，靠人记得写 `defer`。**漏一次 Release，那个 actor 的 inFlight 永远回不到
0，从此再不被回收**，一个用户漏一条协程且不报错。现在它在生成的代码里，想漏也漏不掉。

两份模板都把错误**返回**而不是 `println` 掉 —— 内置模板那种写法会让存储故障变成
"成功返回空结果"，HTTP 层再没机会翻译成 5xx。

### 两条对模块方法的约束

**参数和返回值不能用模块包自己声明的类型。** 门面生成在 `service` 包，gercmd
一律拒绝引用模块包的类型（GameSvr 那种布局下会成环）。这与规范第 1 条是同一件事
的两个说法：跨模块只传 `comm` 里的 `**Snap`。这条约束顺带把签名逼简单了 ——
原先那一堆 `XxxArgs`/`XxxResult` 包装类型只是为了迁就 `ModInvoke` 的 `any`，
去掉之后签名反而更好读。

**要么没有返回值（投递即忘），要么正好是 `(T, error)`。** 违反会让模板生成非法
Go 代码、`gen` 当场失败 —— 这是有意的，早失败好过生成一个悄悄错的门面。

`TestFacadesUpToDate` 调 `gercmd verify -strict` 挡住"改了模块忘了重新生成"，
详见"测试"一节。

## 接口

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| POST | `/api/register` | — | `{"phone","password"}` → `{"uid"}` |
| POST | `/api/login` | — | `{"phone","password"}` → `{"token","uid","expires_in"}` |
| POST | `/api/notes` | Bearer | `{"content"}` → `{"note_id","content","created_at"}` |
| GET | `/api/notes` | Bearer | → `{"count","notes":[…]}`，按上传时间倒序 |
| POST | `/api/mail/send` | **运维令牌** | `{"uid","title","content","items"}` → `{"mail"}` |
| GET | `/api/mails` | Bearer | → `{"count","ready","mails"}`，读用户自己 actor 上的视图 |
| POST | `/api/mails/claim` | Bearer | `{"mail_id"}` → `{"items"}`，直连 MailMgr，结果是权威的 |
| GET | `/healthz` | — | → `{"online_users","discarded"}` 存活的用户 actor 数、累计被丢弃的调用数 |

`/api/mails` 在数据还没从 `MailMgr` 推过来时返回 **202 + `ready:false`** 而不是
200 + 空列表 —— "加载中"和"你没有邮件"在客户端要显示成不同的东西，服务端把它们
混成一个，客户端就再也分不开了。这时 rut 会顺手催 `MailMgr` 推一次。

领取则是 **200 + 权威结果**：它绕过用户侧视图直连 `MailMgr`，返回时存储已经改好。

邮件下发走**运维令牌**（`data/server.json` 的 `ops_token`，或环境变量 `OPS_TOKEN`），
不是用户的 Bearer —— 下发的对象是 UID、操作者是运维，两者是不同身份。令牌没配时
那组接口整体返回 503 而不是敞开：配置漏填是最容易发生的事，而它的后果是任何人都能
给任何用户发道具。

约束：手机号为中国大陆 11 位（`^1[3-9]\d{9}$`，改 `src/security/password.go` 里的
正则即可放开，但先看下面"SQL 拼串"一节）；密码 8 位起、最长 72 字节；
笔记无标题、纯文本，上限 10000 字。`created_at` 是毫秒时间戳。

`uid` 就是手机号——换成 Norm 之后账号主键从自增 id 改成了手机号，原因见下。

## actor 是怎么用的

不是把所有东西都塞进 actor，而是只把**真正需要串行化的状态**交给它。

**注册 → 按手机号分片的 auth actor（`comm.AuthShards` 个）。**
注册是"先查重、再建号"两步，中间有窗口。按手机号哈希分片后同一号码永远落到
同一个 actor，两步天然串行。换成 Norm 之后这一条从"优化"变成了"必需"，见下。

**笔记 → 每个用户一个 actor，按需创建、空闲 5 分钟回收。**
`NoteMod.cache` 是可变状态，但它被单条协程独占，所以业务代码里一把锁都没有，
也不可能出现"多设备并发上传导致缓存与数据库不一致"。慢查询只拖慢他自己，
不影响别人——这是"每用户一个 actor"相对"全局一个 actor"的关键好处。

**会话校验不进 actor。** 纯读，没有需要串行化的状态，绕进去只是白搭一次投递。

### 两条必须守住的纪律

**别把 CPU 密集的活放进事件循环。** bcrypt 是 50~70ms 的纯 CPU 开销，
放进 actor 会把整个分片钉死那么久。所以哈希的计算与比对都在 HTTP 协程完成
（`src/security`），actor 只碰存储。

**别在无返回值的模块方法里 panic。** 框架 `handleTask` 的 recover 在上报
`PhaseInvoke` 之后会顺手 `Close()` 掉整个 actor。为一次记录登录时间的失败
干掉一整个分片，代价完全不成比例——那类错误在模块内部自己落日志消化掉
（见 `AuthMgr.TouchLogin`）。

### 超时语义直接映射到 HTTP

框架区分"确定没执行"和"可能已执行"，这个区分对写操作很关键，别丢掉。
翻译在 `src/bases/invoke.go` 一处完成：

| 框架错误 | HTTP | 含义 |
|---|---|---|
| `ErrTaskCanceled` | 503 | 任务在被取用前就取消了，**方法一次没执行，可直接重试** |
| `ErrTaskAwaitTimeout`（不带取消） | 504 | 方法可能正在执行或已执行完，**重试前需自行去重** |
| `ErrTaskQueueTimeout` | 503 | 队列满，服务繁忙 |
| `ErrCallCycle` | 500 | 跨 actor 调用成环，属服务端编排 bug，不该让客户端重试 |

## 换成 Norm 之后必须知道的四件事

这四条都是 ORM 的语义变化，不是随手可调的参数。

### 1. 主键必须由应用层生成

Norm 的 `Save()` 是"同步写 Redis + 异步入队 MySQL"，拿不回 `LastInsertId`，
自增主键没有回填路径。所以：

- 账号主键直接用手机号（`Account.UID`），顺带省掉一次"按手机号查 id"；
- 笔记主键在 `databases.newNoteID` 里拼成 `uid-毫秒时间戳-6字节随机`。
  随机后缀不能省：同一毫秒内的两次上传会撞主键，而撞主键的表现是
  `ON DUPLICATE KEY UPDATE` **静默覆盖**，最难查的那种丢数据。

### 2. 重复注册没有数据库兜底了

原来靠 `UNIQUE KEY uk_phone` + 1062 错误码兜底。现在写入是异步的
`INSERT ... ON DUPLICATE KEY UPDATE`，唯一冲突既不报错也没有返回值可接，
重复注册的表现变成"后者覆盖前者的密码哈希"。

所以查重完全落在应用层的两道上：auth actor 按手机号分片（同号串行），
以及 `AccountStore.Create` 里进库前的再查一次。单实例部署下这是完备的。

多实例部署仍有一个很窄的窗口——两个进程的分片各自独立。但窗口被 Redis
收窄了：`Save` 写 Redis 是同步的，各实例又共用同一个 Redis，所以 A 的
`Save` 返回之后 B 的 `Find` 就一定看得见，剩下的只有"B 查完但 A 还没写完"
这一小段。**本示例按单实例部署**，真要多实例就得换成 Redis 的 `SETNX` 抢占。

### 3. 超时只能靠 DSN

Norm 的 `Load/Save/FindAll` 内部用 `context.Background()`，不接受外部 ctx，
所以原来那套 `context.WithTimeout(dbTimeout)` 没地方传了。唯一的闸门是
`data/orm.json` 里 DSN 上的 `timeout/readTimeout/writeTimeout=2s`。

这个值必须**明显小于框架写死的 3 秒任务超时**：模块方法跑在事件循环上，
超过 3 秒调用方早已超时走人，actor 还在干等，后面排队的请求全被堵住。

### 4. SQL 拼串

`QueryBuilder.Where(cond string)` 是把 cond 原样拼进 SELECT 的，没有占位符。
`databases/quote.go` 因此用**白名单**（数字、字母、`-`、`_`）而不是转义来处理
查询值，越界直接拒绝。要放开手机号格式（比如支持 `+86` 前缀）之前，
先回头看这个白名单——`+` 不在里面。

## 数据表

表由 Norm 的 AutoMigrate 在进程启动时创建并补列，改字段只要改 Go 结构体上的
`orm` tag，不必写 DDL。`schema.sql` 只负责建库。

| 表 | 主键 | 说明 |
|---|---|---|
| `account` | `uid`（手机号） | 密码哈希、注册/登录时间 |
| `note` | `note_id` | 联合索引 `idx_uid_created (uid, created_at)` |
| `mailbox` | `uid` | **一个用户一条记录**，邮件列表存成 JSON 列 |
| `session` | `token` | **只走 Redis**（`SaveR`/`LoadR`），MySQL 里那张表一直是空的 |

`mailbox` 不是"一封邮件一行"：需求里的"最多 100 封、超了顶掉最老的"是一次**读改写**，
拆成行的话每次下发要查一遍、算出该删哪封、再删一次，而 Norm 的删除是异步软删，
中间那个窗口里信箱可能是 101 封。整个信箱一条记录时这一步就是切片操作，
由 mail 分片的事件循环串行保证。代价是单条记录随邮件数增长——上限算下来最坏
约 800KB，在 MySQL 的 JSON 列里够用；真要"无上限归档"就得改成按行存。

`session` 那张空表是 Norm 的行为，没有"只要 Redis 不要表"的开关，可以无视。

会话有效期受 `data/orm.json` 的 `redis.key_ttl_sec` 控制，不是按对象设的——
`comm.SessionTTL`（7 天）和配置里的 `604800` 必须对齐，改一个就要改另一个。

## 测试

```bash
go test ./... -count=1          # 内存存储，不需要 MySQL/Redis
go test -race ./... -count=1
```

`server_test.go` 用内存存储把 actor 编排和 HTTP 层完整跑通，覆盖并发注册同一号码
（只能成功一个）、同账号并发上传（缓存与存储一致）、空闲回收后数据不丢、
使用中的 actor 不被回收、800 汉字原样往返、存储故障必须变成 5xx 等。

被测的是真正跑在线上的那套路由和编排——只有 `contract` 的三个实现被换掉了。

`layering_test.go` 把上面"项目规范"里的依赖表、命名规则、以及"模块间只能单向通知"
（`TestModsInterfacesAreNotifyOnly`）跑成断言：包引了不该引的
东西、功能文件夹里出现第二个 `**_mod.go`、路由文件夹缺 `**_rut.go`、comm 下的文件
少了 `_comm` 后缀、comm 里导出的 struct 没以 `Snap` 结尾，都会红。
新建包时要同时在它的表里登记依赖约束，否则"包不在分层表里"也会红。

`generate_test.go` 挡住生成的门面与模块漂移，做法是调 `gercmd verify -strict`：

```bash
go -C ../.. run ./cmd/gercmd verify -strict cmd/noteserver/src
```

它**自己**递归找 `**_mod.go` / `**_mgr.go`、自己读它们 `//go:generate` 里的参数、
自己重新生成比对，还会反过来报告没有模块对应的多余 `**_export.go`。这里刻意不在
测试里列清单——那张清单本身就是新的人为出错点：加了模块忘了登记，检查就默默漏掉
它。**检查工具自己需要人来维护，等于没有检查。**

`-strict` 拦的是"**漏写了 `//go:generate` 指令**"——本项目要求每个模块都能被校验。
它**不会**因为某个模块没有 `export:` 标记就报错：那是合法设计（见规范第 7 条），
所以加一个纯内部模块不会让这条测试变红。
自动路由本身的单测与基准跟着代码搬到了 `web/` 模块（`cd ../../web && go test ./...`）。
`src/databases/quote_test.go` 单测那个 SQL 白名单，`src/security/password_test.go`
单测手机号与密码规则。这几个包都不需要任何外部依赖。

## 压测

压测默认跳过，用环境变量打开：

```bash
NOTE_STRESS=1 go test -run TestStress -v -count=1                 # 内存存储
NOTE_STRESS=1 NOTE_STRESS_REAL=1 go test -run TestStress -v       # 真实 MySQL+Redis
NOTE_STRESS=1 go test -race -run TestStress -count=1              # 查数据竞争
```

两种模式压的不是同一件事。内存模式把存储耗时压到接近 0，剩下的全是 actor 编排、
HTTP 与反射调用的开销——它回答"框架本身能扛多少"。真实模式加进 Norm 的 Redis
同步写与 MySQL 异步刷盘，回答"这套编排在真实延迟下会不会塌"。

五个用例，各压一条路径：

| 用例 | 压什么 | 卡在哪 |
|---|---|---|
| `RegisterStorm` | auth 分片扛不扛得住注册洪峰 | **bcrypt，不是 actor** |
| `SamePhoneRegister` | 512 次并发注册同一号码 | 必须恰好 1 次成功 |
| `UploadThroughput` | 每用户一个 actor 的写入与缓存一致性 | 结束后逐用户核对条数 |
| `EvictionUnderLoad` | 边打流量边回收 actor | 一次失败都不允许 |
| `GoroutineSettles` | 关完之后事件循环有没有退出 | 协程数要落回基线 |

28 核机器上的实测（内存 / 真实存储）。吞吐这一列的散布很大，只该当数量级看，
不能拿来比较两次改动的优劣——原因见下面第四条：

| | 内存 | 真实 MySQL+Redis |
|---|---|---|
| 注册 | ~540 req/s，p99 0.4~0.6s | ~465 req/s，p99 196ms |
| 上传 | 34~48k req/s，p50 1.0~1.5ms | 13.5~14.2k req/s，p50 3.3ms p99 13.7ms |
| 读取 | ~42k req/s，p50 1.0ms | 13.5~18.5k req/s，p50 2.2ms |
| 回收压测 | ~29k req/s，回收 655 个 actor，0 失败 | ~15k req/s，回收 1036 个，0 失败 |

四条值得记下来的结论：

**注册的瓶颈是 bcrypt，不是 actor。** 上传能跑到 48000 req/s，注册只有 550 —— 差
两个数量级，因为每次注册要在 HTTP 协程上算一次 ~60ms 的 bcrypt，28 核满打满算也就
`28/0.06 ≈ 466` req/s。这正是把 bcrypt 留在 HTTP 层的效果：它吃满了 CPU，但一条
事件循环都没被它占住。真要提注册吞吐，唯一的旋钮是 `bcrypt.DefaultCost`。

**回收与请求并发时零失败。** 巡检把"当前时间"推到 24 小时后，每毫秒扫一遍，
整轮回收掉六百到一千个 actor，同时 2000 个请求在打——`inFlight` 计数守住了，
没有一个请求撞上已关闭的 actor。

**异步刷盘一条没丢。** 真实模式跑完，MySQL 里 `note` 表正好是本轮上传数
（400 + 667 + 300 = 1367），`session` 表 0 行——后者符合设计，会话只走 Redis。
`-race` 下五个用例全过，零数据竞争。

**这套压测量不了微观开销，别拿它当基准。** 同一份代码连跑四次，上传吞吐是
47572 / 43731 / 43562 / 34451 req/s——38% 的散布，p50 还在 1.0ms 与 1.5ms 之间跳
（调度与计时粒度）。想量"某处改动值多少钱"必须用 `src/bases/router_bench_test.go`
那种隔离基准，否则读到的全是噪声。

## 本机联调的一个坑

若 shell 里设了 `HTTP_PROXY`/`HTTPS_PROXY`，curl 打 `127.0.0.1` 也会绕经代理，
表现是**请求体里的 UTF-8 被改写、稍大的载荷直接 502**。联调时务必绕开：

```bash
curl --noproxy '*' ...
# 或
unset HTTP_PROXY HTTPS_PROXY
```

## 待办

`go.mod` 里 `replace github.com/norm => D:/cloud/Norm` 是本机绝对路径。
Norm 打上 tag 之后要换成正式版本号并删掉这行——在此之前，这个示例在你本机
之外构建不了。
