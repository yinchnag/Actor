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

## 目录结构

```
main.go              只做装配：读配置 → 起连接池 → 建 Hub → 挂路由 → Run
data/                两份配置
src/
  bases/             全局 gin 引擎、应答格式、actor 错误 → HTTP 语义、web.Router 的选项
  comm/              跨层共享常量（会话有效期、笔记上限、分片数…）
  config/            服务器自身配置
  contract/          接口与错误哨兵，不放实现
  databases/         Norm 表结构 + contract 的生产实现
  middleware/        Bearer 鉴权
  mods/              actor 模块：AuthMod、NoteMod（门面由 export: 标记生成）
  router/            按业务分包，每包一个 xxx.go + xxx_type.go
    auth/ note/ health/
  security/          bcrypt、令牌、手机号与密码校验
  service/           Hub：actor 的创建与回收 + 生成的门面（*_export.go）
templates/           门面的生成模板
```

与参考项目 `roleSvr` 的一点不同：**存储藏在 `contract` 的接口后面**。`roleSvr` 的
service 直接调 databases，那是业务服务器的写法；这里多一层接口，是为了让整套 actor
编排能在**没有 MySQL/Redis** 的机器上被完整测到（见下面"测试"一节）。示例跑不起
测试就失去了示例的意义。

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

actor 模块放在 `src/mods/`，遵循仓库的模块规范 —— `gercmd check` 能验：

```bash
go -C ../.. run ./cmd/gercmd check auth cmd/noteserver/src   # 3/3 通过
go -C ../.. run ./cmd/gercmd check note cmd/noteserver/src
```

规范三条：struct 内嵌 `actor.ModObj[*自己]`；构造函数返回 `actor.IModule`；
要对外暴露的方法带 `export:` 标记。

路由**不直接调 `ModInvoke`**。方法上的标记会被生成成 `Hub` 上的门面：

```go
//	export: NoteAdd
func (that *NoteMod) Add(content string) (contract.NoteInfo, error)
```

↓ `go generate ./src/mods/`

```go
// src/service/note_export.go — Code generated; DO NOT EDIT.
func (that *Hub) NoteAdd(gid uint64, uid string, content string) (contract.NoteInfo, error) {
	loader := that.AcquireUser(uid)
	defer that.ReleaseUser(uid)
	out, err := loader.ModInvokeFrom(gid, "NoteMod", "Add", content)
	return unwrap[contract.NoteInfo](out, err, "NoteMod.Add")
}
```

于是路由那边是 `that.hub.NoteAdd(gid, uid, content)` —— 一个字符串字面量都没有，
改了模块方法名，路由**编译就红**，不必等运行期的 `module not found`。

### 为什么要自己写模板

`gercmd` 的内置模板是为 GameSvr 那种布局写的（宿主唯一、`that.GetModloader()`、
错误 `println` 掉）。noteserver 三处对不上，所以 `templates/` 下自带两份：

| 模板 | 用于 | 关键差别 |
|---|---|---|
| `shard_export.tmpl` | AuthMod | loader 按手机号分片取：`that.AuthFor(uid)` |
| `user_export.tmpl` | NoteMod | `AcquireUser` / `defer ReleaseUser` 收进门面 |

第二条是这次改造最实在的收获：取用与归还原先写在每个 handler 里，靠人记得写
`defer`。**漏一次 Release，那个 actor 的 inFlight 永远回不到 0，从此再不被回收**，
一个用户漏一条协程。现在它在生成的代码里，想漏也漏不掉。

两份模板都把错误**返回**而不是 `println` 掉 —— 内置模板那种写法会让存储故障变成
"成功返回空结果"，HTTP 层再没机会翻译成 5xx。

### 两条对模块方法的约束

**参数和返回值不能用 `mods` 包自己声明的类型。** 门面生成在 `service` 包，
gercmd 一律拒绝引用模块包的类型（GameSvr 那种布局下会成环）。跨层值类型统一放
`contract`。这条约束顺带把签名逼简单了 —— 原先那一堆 `XxxArgs`/`XxxResult`
包装类型只是为了迁就 `ModInvoke` 的 `any`，去掉之后签名反而更好读。

**要么没有返回值（投递即忘），要么正好是 `(T, error)`。** 违反会让模板生成非法
Go 代码、`gen` 当场失败 —— 这是有意的，早失败好过生成一个悄悄错的门面。

`TestGeneratedFacadesUpToDate` 会把生成器再跑一遍跟磁盘上的文件比，
挡住"改了模块签名忘了重新生成"。

## 接口

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| POST | `/api/register` | — | `{"phone","password"}` → `{"uid"}` |
| POST | `/api/login` | — | `{"phone","password"}` → `{"token","uid","expires_in"}` |
| POST | `/api/notes` | Bearer | `{"content"}` → `{"note_id","content","created_at"}` |
| GET | `/api/notes` | Bearer | → `{"count","notes":[…]}`，按上传时间倒序 |
| GET | `/healthz` | — | → `{"online_users","discarded"}` 存活的用户 actor 数、累计被丢弃的调用数 |

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
（见 `AuthMod.TouchLogin`）。

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
| `session` | `token` | **只走 Redis**（`SaveR`/`LoadR`），MySQL 里那张表一直是空的 |

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

`generate_test.go` 挡住生成的门面与模块签名漂移。
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
