# gercmd

配合本仓库 actor 框架使用的命令行工具。两件正事——**校验模块是否符合 GameSvr 的
模块规范**、**为模块方法生成 player 包的门面函数**；外加三个顺手的文件命令。

```bash
go run ./cmd/gercmd <命令> [选项] [参数]

# 或先编出来
python cmd/gercmd/build.py           # 产物落在 cmd/gercmd/
python cmd/gercmd/build.py --test    # 先跑测试再编
python cmd/gercmd/build.py --all     # 常见平台各编一份
```

| 命令 | 干什么 |
|---|---|
| `check` | 检查模块是否符合模块规范 |
| `gen` | 为模块的公有方法生成门面函数 |
| `verify` | 校验生成的门面是否还与模块一致 |
| `dirs` | 列出路径下的文件夹 |
| `files` | 列出路径下的文件 |
| `cat` | 打印文件内容 |

各命令的选项用 `gercmd <命令> -h` 查看。

---

## check —— 模块规范闸门

```bash
gercmd check bag cmd/GameSvr        # 名字给 bag / Bag / bagMod / BagMod 都行
gercmd check auth cmd/noteserver/src # 分不清 Mod 还是 Mgr？候选都试一遍
gercmd check bag                    # 不给路径就在当前目录下递归找
```

```
检查 BagMod（来自 "bag"），范围 cmd/GameSvr

  ✓  struct BagMod                        cmd/GameSvr/bag/bag_mod.go:14
  ✓  内嵌 actor.ModObj[*BagMod]           cmd/GameSvr/bag/bag_mod.go:16
  ✓  func NewBagMod(...) actor.IModule    cmd/GameSvr/bag/bag_mod.go:8

公有方法 2 个，其中导出函数 2 个（→ 表示导出，· 表示模块内部方法）:
  →  AddItem          BagAddItem           cmd/GameSvr/bag/bag_mod.go:21
  →  RemoveItem       BagRemoveItem        cmd/GameSvr/bag/bag_mod.go:27

3/3 通过
```

三项全过退出码 0，任一项不过退出码 1——可以直接放进 CI 当闸门。

### 这三项各自拦的是什么

**`struct BagMod` 存在。** 框架按类型名注册模块，`ModInvoke("BagMod", ...)`
里的字符串就是它。

**内嵌 `actor.ModObj[*BagMod]`，类型参数必须是自己。** 这条最值钱。写成
`ModObj[*HeroMod]` 编译得过、不报错，但 `ModObj` 靠指针偏移反推宿主对象，
类型参数不对就会反推到别的类型上，模块名变成空串。运行期的表现是**这个模块
的功能就是没反应**，而框架层只会给你一句 `module not found`。

**`func NewBagMod(...) actor.IModule` 恰好一个返回值。** 构造函数必须是普通函数
不能是方法，返回值必须是接口而不是具体类型。

### 顺带报出来的（不计入三项判定）

- **同名 struct 的其它定义位置**——框架按类型名注册，两个 `BagMod` 会抢同一个
  key，`AddModule` 时后挂的直接覆盖先挂的，一声不吭
- **大小写写错的 `export:` 标记**（`Export:`、`EXPORT:`）——静默忽略的话方法会
  悄悄不算导出函数，正是这工具要拦的那类问题
- **私有方法上带了 `export:` 标记**——框架只反射公有方法，它永远不会被调用到
- **用了包别名**（`import a "actor"`）——不算错，但提醒一句免得看的人以为写错了
- **解析失败的文件**——不能让「没找到」和「没解析成」混为一谈

---

## gen —— 生成门面函数

模块跑在 actor 的事件循环上，外部只能通过 `ModInvoke` 反射调用它——字符串方法名、
`[]reflect.Value` 返回值、错误得自己接。门面函数就是把这套样板封在 `player` 包里，
让调用方写 `p.BagAddItem(1001, 5)` 而不是手拼反射调用。

```bash
gercmd gen -n cmd/GameSvr/bag/bag_mod.go cmd/GameSvr/player     # 先预览
gercmd gen cmd/GameSvr/bag/bag_mod.go cmd/GameSvr/player        # 落盘
gercmd gen -force ...                                            # 覆盖已有文件
```

输出文件名剥掉末尾的 `_mod` / `_mgr`，再加 `_export`：

    bag_mod.go   → bag_export.go
    auth_mgr.go  → auth_export.go

两个后缀都认：模块按"挂在用户身上还是挂在服务器上"分成 `**_mod.go` 与 `**_mgr.go`，
但生成的门面没有这个区别——都是挂在同一个宿主上的普通方法，文件名里再带 mod/mgr
只会让人以为那是两类东西。不合命名约定的文件一律退化成加后缀。
**默认不覆盖已存在的文件**——门面文件里往往有手写代码，静默覆盖等于毁掉别人的工作。

### export: 标记

模块方法用 doc 注释里的标记声明它对应的门面函数名：

```go
// 增加物品
//	export: BagAddItem
func (that *BagMod) AddItem(itemid, count int) {}
```

生成：

```go
// 增加物品
func (that *PlayerEnt) BagAddItem(itemid int, count int) {
	if _, err := that.GetModloader().ModInvoke("BagMod", "AddItem", itemid, count); err != nil {
		println("BagAddItem err:", err.Error())
	}
}
```

`export: X` 和 `export:X` 都认。没带标记的公有方法就只是模块内部方法，不对外——
想给所有公有方法都生成，加 `-all`。

### 三类方法会被跳过（原因会打到 stderr，不会闷声少生成）

| 情况 | 为什么 |
|---|---|
| 没有 `export:` 标记 | 默认行为，加 `-all` 可以覆盖 |
| 有可变参数 | 框架的反射调用不支持 |
| 参数或返回值用了模块包自己声明的类型 | 门面在 `player` 包里，要用它就得 import 模块包，而模块包又 import 了 `player`——直接成环，生成出来也编译不过 |

最后一条的解法是把类型挪到第三方包（或 `player` 包）里，别放模块包。

### 换模板

输出形态由模板决定，不焊死在 Go 代码里。默认模板内嵌自
`templates/export.tmpl`，用 `-tmpl <文件>` 换成自己的；`-recv` 换接收者类型名
（默认 `PlayerEnt`）。想把 `println` 换成项目日志、想加埋点、想让门面把 `error`
传出去——改模板就行，不用碰生成器也不用重新编译。

模板里有三处不能省，省了会编译不过或运行时 panic（文件顶部的注释里写明了）：
有返回值时零值变量必须提前声明、返回值个数校验要独立于 `err`、类型断言一律用
逗号 ok 形式。改完跑一遍：

```bash
go test ./cmd/gercmd/ -run TestGenCompiles      # 把生成结果真编译一遍
```

---

## 命名约定可配置

gercmd 同时服务两支项目模板 —— 游戏服务器照 `cmd/GameSvr`、Web 服务器照
`cmd/noteserver`，它们的约定不完全一样，以后还会有第三支。所以**约定不写死在
代码里**，集中在 `naming.go`，并且全部开成参数（`check` / `gen` / `verify` 都认）：

| 参数 | 默认值 | 管什么 |
|---|---|---|
| `-mod-suffix` | `_mod,_mgr` | 模块文件后缀 |
| `-type-suffix` | `Mod,Mgr` | 模块类型名后缀 |
| `-export-suffix` | `_export` | 生成物文件后缀 |
| `-ctor-prefix` | `New` | 构造函数前缀 |
| `-recv` | `PlayerEnt` | 门面方法的接收者类型（`gen` / `verify`） |

默认值取两支模板的**并集**，于是它们都不必传参。约定完全不同的项目传参即可：

```bash
# 一个用 **_service.go / **Service / MakeXxx / **_api.go 的项目
gercmd check -type-suffix Service -ctor-prefix Make shop ./src
gercmd gen   -mod-suffix _service -export-suffix _api -recv Gateway              src/logic/shop_service.go src/facade
gercmd verify -mod-suffix _service -export-suffix _api ./src
```

`-recv` 的默认值 `PlayerEnt` 取自 GameSvr 那一支，**只是个默认值不是规定**：
noteserver 的宿主是 `Hub`，它所有 `//go:generate` 里都显式写了 `-recv`。

> 路径本身从来不写死——全部来自命令行参数。写死过的是这些**命名约定**，
> 而且早先只照顾了 GameSvr 一支：`check` 无条件给名字补 `Mod` 后缀，
> 于是 `check auth` 去找 `AuthMod`、找不到 noteserver 的 `AuthMgr`。

### check 会试所有候选

`check auth` 分不清你指的是 `AuthMod`（挂用户）还是 `AuthMgr`（挂服务器）——
名字里没有这个信息。所以它把候选都试一遍，哪个的 struct 真的存在就检查哪个；
都不存在时报第一个，并在提示里列出全部候选：

```
✗  struct AuthMod
   没有找到 struct AuthMod（候选：AuthMod / AuthMgr）
```

"名字被占了但不是 struct"这类更具体的诊断优先于候选提示 —— 那种情况下用户
需要的是"你把 BagMod 定义成别的东西了"，而不是一句"没找到"。

## verify —— 生成物是否还是最新的

```bash
gercmd verify cmd/noteserver/src          # 递归校验
gercmd verify -strict cmd/noteserver/src  # 把"模块没有 go:generate"也算失败
```

它抓的是一类**编译得过**的错，所以人和编译器都发现不了：

- 改了模块方法的签名，忘了重新生成，门面还是旧的；
- 改了模块文件名（`auth_mod.go` → `auth_mgr.go`），生成物换了名字，
  旧的那份留在原地继续参与编译，内容是对的、只是过期了；
- 删了一个模块，它的门面还在。

做法是**自己发现，不要清单**：

1. 递归找 `**_mod.go` / `**_mgr.go`；
2. 读它们各自 `//go:generate` 里的 `gercmd gen` 参数 —— 模板、接收者、输出目录
   本来就全写在那儿，那是唯一的事实来源；
3. 按同样的参数重新生成一遍（只在内存里），与磁盘上的文件比；
4. 反过来扫输出目录，报告没有任何模块对应的多余 `**_export.go`。

> 早先这件事是让使用方在自己的测试里手写一张"模块 → 模板 → 生成物"的表。
> 那张表本身就是新的人为出错点：加了模块忘了登记，检查就默默漏掉它。
> **检查工具自己需要人来维护，等于没有检查**，所以改成现在这样。

`//go:generate` 里的 `-C` 会被还原：`go -C ../.. run ./cmd/gercmd gen ...` 里的路径
是相对 `-C` 那个目录的，verify 照着算，于是在哪个目录下跑都对得上。

判定与退出码：

| 状态 | 含义 | 计入失败 |
|---|---|---|
| `✓ ok` | 生成物与模块一致 | |
| `· noexport` | 模块没有 `export:` 标记，正确地不产生生成物 | **否** |
| `✗ stale` | 内容对不上，跑一次 `go generate` | 是 |
| `✗ missing` | 该有生成物却没有 | 是 |
| `✗ leftover` | **不该**有生成物却有——标记删了、文件没删 | 是 |
| `✗ error` | 指令解析或生成失败 | 是 |
| `✗` 残留 | 输出目录里没人认领的 `**_export.go` | 是 |
| `· nodirective` | 模块没有 `//go:generate`，无从校验 | 仅 `-strict` |

### noexport 与 nodirective 必须分开

两者都表现为"没有生成物"，含义却**相反**：

- `noexport` 是**校验过了**——生成器跑了，结论是这个模块不该产生门面。
  模块只有内部方法是合法设计，所以它**永远不算失败**，`-strict` 也不升级它。
- `nodirective` 是**压根没校验**，多半是漏写了指令。`-strict` 就是为它设的。

混成一个状态的话，`-strict` 想拦第二种就会连第一种一起误伤：一个合法的纯内部
模块能让整个校验变红。`cmd/noteserver` 的 `TestFacadesUpToDate` 跑的正是
`verify -strict`，混判会让它一加内部模块就失败。

`nodirective` 默认不算失败，是为了不误伤手工生成门面的项目（`cmd/GameSvr` 就是）。

### leftover 为什么不交给孤儿检查

"把方法上的 `export:` 删了、忘了删生成物"这一步很常见，而那份残留躲得过孤儿
检查：模块还在，它的输出路径就还登记在 `expected` 里，等于给残留打掩护。
反过来把模块文件整个删掉倒能查出来——也就是说漏洞开在**更常见**的那条路上。

所以这一判定放在 `verifyOne` 里而不是 `findOrphans`。诊断也只有在那里才说得准：
孤儿检查那句"多半是改了模块文件名之后忘了删旧的生成物"在这个场景下是错的，
会把人引向改文件名的方向。

## dirs / files / cat

```bash
gercmd dirs cmd                     # 列出 cmd 下的文件夹
gercmd files -r -skip-hidden .      # 递归列出所有文件，跳过 .git 之类
gercmd cat -n cmd/gercmd/main.go    # 带行号打印
```

`dirs` / `files` 默认只看直接子条目、包含隐藏条目、不跟随符号链接；`-r` 递归
（此时输出相对路径而不是名称）。

`cat` 按字节原样输出，**不做编码转换**——文件是什么编码出来就是什么编码。在 GBK
控制台上看 UTF-8 文件会花屏，那是终端的事，一转就没法保证重定向到文件时字节不变。
看着像二进制的文件会拒绝打印（判据是前若干字节里有 NUL，和 git 用的启发式一样），
确认要看加 `-f`。

---

## 输出与退出码约定

**结果走 stdout，统计与告警走 stderr。** 所有命令一致，所以可以直接重定向或接管道
而不会混进杂音：

```bash
gercmd files -r . > filelist.txt        # 拿到的是干净的文件列表
gercmd cat x.go > y.go                  # 拿到的是字节一致的副本
```

| 退出码 | 含义 |
|---|---|
| 0 | 正常 |
| 1 | 干活时出错（路径不存在、check 不通过等） |
| 2 | 用法错误（命令写错、参数个数不对） |

分开是为了让脚本能区分「跑了但结果是坏的」和「我命令写错了」。

---

## 加一个子命令

每个子命令的实现和它的入口函数放在同一个文件里（`list.go` / `cat.go` /
`check.go` / `gen.go`），共用的部分在 `cli.go`（FlagSet、参数校验、输出约定）
与 `walk.go`（目录遍历）。

**加新功能 = 加一个文件 + 在 `main.go` 的命令表里加一行**，不动已有子命令。

---

## 测试

```bash
go test ./cmd/gercmd/ -count=1
```

几条值得知道的：

- **`TestGenCompiles`** —— 这个功能唯一不能省的测试。造一个自带 `go.mod` 的临时
  模块，把生成结果**真编译一遍**。参数名遮蔽内建类型、返回值零值没声明、类型渲染
  漏了包名，这几类问题肉眼很难发现，编译器一看一个准。
- **`TestGeneratedFacadesUpToDate`** —— 入库的 `cmd/GameSvr/player/*_export.go`
  必须与当前生成器的产出一致。生成物漂移是没有症状的：编译照过、测试照绿，只有下次
  重新生成时才冒出一堆没人认得的 diff。这条同时是 AST 层的回归网，动模板或换解析库
  只要产出变了就先红。
- **`TestCheckRealGameSvr` / `TestExportRealGameSvr`** —— 拿仓库里真实的 GameSvr
  模块跑一遍。规范校验器要是对自家规范的样板都判错，那就没意义了。
- **`TestVerify*`** —— verify 的价值全在"能不能抓到那几种编译得过的错"，所以整组
  测试都围绕失败场景造：过期、缺失、残留、指令缺失、`-C` 还原。只验"正常情况通过"
  没有意义。
- **`TestNamingDefaultsCoverBothTemplates` / `TestWithDefaultsIsPerField`** ——
  前者保证默认约定同时覆盖 GameSvr 与 noteserver 两支模板（只照顾一支的话，
  另一支每条命令都得带参数）；后者钉住一个真踩过的坑：只改一项约定时，
  其余项要退回默认而不是把这一项也一起吞掉。

## 依赖

AST 解析用 [`github.com/yinchnag/GCore/ast`](https://github.com/yinchnag/GCore)。
当前 `go.mod` 里用 `replace` 指向本地检出：

```
replace github.com/yinchnag/GCore => D:/cloud/GCore
```

这是开发期的临时安排——**换一台机器就 build 不了**。GCore 打上 tag 之后换成正式
版本号并删掉这行。
