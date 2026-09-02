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
| `dirs` | 列出路径下的文件夹 |
| `files` | 列出路径下的文件 |
| `cat` | 打印文件内容 |

各命令的选项用 `gercmd <命令> -h` 查看。

---

## check —— 模块规范闸门

```bash
gercmd check bag cmd/GameSvr        # 模块名给 bag / Bag / bagMod / BagMod 都行
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

输出文件名把 `mod` 换成 `export`：`bag_mod.go` → `bag_export.go`。
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

## 依赖

AST 解析用 [`github.com/yinchnag/GCore/ast`](https://github.com/yinchnag/GCore)。
当前 `go.mod` 里用 `replace` 指向本地检出：

```
replace github.com/yinchnag/GCore => D:/cloud/GCore
```

这是开发期的临时安排——**换一台机器就 build 不了**。GCore 打上 tag 之后换成正式
版本号并删掉这行。
