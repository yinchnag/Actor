# noteserver

基于本仓库 actor 框架的笔记服务：手机号注册、登录、上传笔记、获取笔记。

## 起服务

```bash
mysql -uroot -p < cmd/noteserver/schema.sql   # 建库建表
go run ./cmd/noteserver                        # 默认读项目根目录的 .env
go run ./cmd/noteserver -env /path/to/.env     # 或指定路径
```

`.env` 只有四个键——监听地址、端口、MySQL、Redis。其余参数（会话有效期、
空闲回收间隔、笔记字数上限）是程序行为而不是部署配置，写在代码常量里。

环境变量优先于 `.env`，所以线上不必改文件：

```bash
SERVER_PORT=18080 go run ./cmd/noteserver
```

## 接口

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| POST | `/api/register` | — | `{"phone","password"}` → `{"user_id"}` |
| POST | `/api/login` | — | `{"phone","password"}` → `{"token","user_id","expires_in"}` |
| POST | `/api/notes` | Bearer | `{"content"}` → `{"id","content","created_at"}` |
| GET | `/api/notes` | Bearer | → `{"count","notes":[…]}`，按上传时间倒序 |
| GET | `/healthz` | — | → `{"online_users"}` 当前存活的用户 actor 数 |

约束：手机号为中国大陆 11 位（`^1[3-9]\d{9}$`，改 `http.go` 里的正则即可放开）；
密码 8 位起、最长 72 字节（bcrypt 硬限制，超过的部分会被它忽略，所以显式拒绝）；
笔记无标题，纯文本，上限 20000 字——需求要求的 800 汉字是 2400 字节，余量很大。

## actor 是怎么用的

不是把所有东西都塞进 actor，而是只把**真正需要串行化的状态**交给它。

**注册 → 按手机号分片的 auth actor（`authShards` 个）。**
注册是"先查重、再插入"两步，中间有窗口。按手机号哈希分片后同一号码永远落到
同一个 actor，两步天然串行，不靠数据库唯一索引也不会有两个请求同时通过查重。
分成多片则是别让所有注册挤在一条事件循环上。唯一索引仍然保留——多实例部署时
各进程的分片互相独立，全局唯一只能靠数据库兜。

**笔记 → 每个用户一个 actor，按需创建、空闲 5 分钟回收。**
`NoteMod.cache` 是可变状态，但它被单条协程独占，所以业务代码里一把锁都没有，
也不可能出现"多设备并发上传导致缓存与数据库不一致"。慢查询只拖慢他自己，
不影响别人——这是"每用户一个 actor"相对"全局一个 actor"的关键好处。

**会话校验不进 actor。** 纯读，没有需要串行化的状态，绕进去只是白搭一次投递。

### 两条必须守住的纪律

**别把 CPU 密集的活放进事件循环。** bcrypt 是 50~70ms 的纯 CPU 开销，
放进 actor 会把整个分片钉死那么久。所以哈希的计算与比对都在 HTTP 协程完成，
actor 只碰数据库。

**数据库操作必须显著快于框架的任务超时。** 框架的 `defaultTaskTimeout` 写死 3 秒，
而模块方法跑在事件循环上——超过 3 秒，调用方早已超时走人，actor 还在干等，
后面排队的请求全被堵住。所以 `dbTimeout` 设为 2 秒，`.env` 里的 DSN 也带了
`timeout/readTimeout/writeTimeout=2s`。

### 超时语义直接映射到 HTTP

框架区分"确定没执行"和"可能已执行"，这个区分对写操作很关键，别丢掉：

| 框架错误 | HTTP | 含义 |
|---|---|---|
| `ErrTaskCanceled` | 503 | 任务在被取用前就取消了，**方法一次没执行，可直接重试** |
| `ErrTaskAwaitTimeout`（不带取消） | 504 | 方法可能正在执行或已执行完，**重试前需自行去重** |
| `ErrTaskQueueTimeout` | 503 | 队列满，服务繁忙 |
| `ErrCallCycle` | 500 | 跨 actor 调用成环，属服务端编排 bug，不该让客户端重试 |

## 测试

```bash
go test ./cmd/noteserver/ -v          # 内存存储，不需要 MySQL/Redis
go test -race ./cmd/noteserver/
```

`server_test.go` 用内存存储把 actor 编排和 HTTP 层完整跑通，覆盖并发注册同一号码
（只能成功一个）、同账号并发上传（缓存与存储一致）、空闲回收后数据不丢、
使用中的 actor 不被回收、800 汉字原样往返等。

## 本机联调的一个坑

若 shell 里设了 `HTTP_PROXY`/`HTTPS_PROXY`，curl 打 `127.0.0.1` 也会绕经代理，
表现是**请求体里的 UTF-8 被改写、稍大的载荷直接 502**。联调时务必绕开：

```bash
curl --noproxy '*' ...
# 或
unset HTTP_PROXY HTTPS_PROXY
```
