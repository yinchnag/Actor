# CRTP：奇异递归模板模式在 Go 模块系统中的实现

## 1. 概述

CRTP 的全称是 **Curiously Recurring Template Pattern**，中文通常译为“奇异递归模板模式”。它最早常见于 C++，典型写法是：

```cpp
template <typename Derived>
class Base {
public:
    void call() {
        static_cast<Derived*>(this)->run();
    }
};

class Child : public Base<Child> {
public:
    void run() {}
};
```

`Child` 继承自 `Base<Child>`，也就是把自己的类型作为参数传给“基类”。这样，基类模板就可以在编译期知道派生类的具体类型。

Go 没有传统意义上的类继承，也没有 C++ 那样的模板。不过，我可以借助以下语言特性组织出相近的结构：

- 泛型：让通用类型接收具体模块类型作为类型参数；
- 结构体嵌入：复用通用模块能力；
- 反射：发现具体模块的公开方法；
- `unsafe`：根据字段偏移找回外层模块对象。

我在这篇文章讨论的 Actor 模块框架中的 `ModObj[T]` 就是这种 **CRTP-like 实现**。它与 C++ CRTP 的实现机制不同，属于“Go 泛型 + 组合 + 反射”形成的同类设计。更准确地说，C++ CRTP 提供了思路，我在 Go 中则把这套思路转换成适合 Go 类型系统和运行时机制的模块基座。

2023 年，我在一家游戏公司参与底层系统从零到一的建设。那段经历让我很清楚地认识到：多人协作时，项目标准不能只停留在文档、约定和口头提醒上，更可靠的方式是尽可能把正确的使用方式落实到代码结构和 API 边界中。

这里说的“在代码层面封死”，重点是为团队提供清晰、公开的使用边界。我更希望公共代码方便同事阅读和维护。对于容易误用的流程，我会尽量减少自由组合的空间。例如，模块名称不应由每个调用者重复传入，反射注册不应依赖每个业务模块记住调用顺序，宿主对象也不应通过没有约束的 `interface{}` 在底层传递。这样做的目的，是让多人开发时少遇到难以定位的运行时错误，把更多精力放在业务本身上。

我当时并没有对 C++ 的 CRTP 做过系统、深入的理论研究，只是在实际开发中接触到“通用基类接收具体类型”这个概念，并直观地感受到它对约束模块结构、减少重复代码很有帮助。后来将类似思想带到 Go 中时，我才把它和结构体组合、泛型、反射方法注册以及 Actor 模块调度结合起来。因而，我写这篇文章主要记录一次工程实践，借此说明如何借鉴一个 C++ 概念，改善底层框架与上层业务之间的边界。

我还需要特别区分两件事：C++ CRTP 能在编译期确定具体类型，并以此实现静态多态；我这里的 Go 实现仍然依赖运行时反射来发现和调用方法，因此不能宣称获得了 C++ CRTP 的全部特性。我真正希望保留的收益，是通过 `ModObj[T]` 明确模块与通用基座的关系，再由框架统一完成初始化和注册，从结构上减少业务代码的重复步骤和误用机会。

## 2. Actor 模块框架要解决的问题

Actor 框架把一个业务对象拆分成多个模块。例如，一个玩家可以拥有背包模块和英雄模块，这些模块由同一个 `ActorLoader` 管理。

每个模块都需要具备一组相同的基础能力：

- 被 ActorLoader 注册和管理；
- 拥有模块名称；
- 通过方法名被调用；
- 自动发现公开业务方法；
- 自动识别协议消息处理方法；
- 支持保存、加载和脏标记。

如果每个模块都手动实现这些逻辑，代码会重复，而且新增业务模块时容易遗漏注册步骤。我更倾向于把这类稳定、重复的流程交给 `ModObj[T]`，让业务模块只需要声明自己的状态和方法。

## 3. 从早期 KingServer 实现的硬编码看问题

在早期 KingServer 的区域服务器中，兵器模块的典型写法大致如下：

```go
type ArmouryMod struct {
	player      *player.PlayerObj
	data        SQL_Armoury
	armouryBag  ArmouryBag
	taskHandler *ArmouryTaskHandler
	core.ModObj
}

func (mod *ArmouryMod) Init(name string, host interface{}) core.IModule {
	mod.player = host.(*player.PlayerObj)
	mod.data.Init(&mod.data)
	mod.ModObj.Init(name, host)
	mod.SetInvokerAll(mod)
	mod.armouryBag.Init(mod)
	mod.player.BagMod_RegisterBag(&mod.armouryBag)
	return mod
}
```

这段代码可以运行，也能够承载实际业务，但它把通用模块框架和兵器业务初始化绑在了一起。`Init` 同时处理框架元数据、`SQL_Armoury`、`ArmouryBag` 以及玩家模块注册，因此两类初始化难以单独演进。

早期接口还让上层业务参数渗透到底层。底层模块基类要求接受下面的参数：

```go
Init(name string, host interface{})
```

`host.(*player.PlayerObj)` 的类型断言只能在运行时检查。模块名称也依赖调用方传入，名称错误要到运行时才会暴露。每个新模块还要重复编写相同的初始化和反射绑定代码。

早期模块还要手工触发框架步骤：

```go
mod.ModObj.Init(name, host)
mod.SetInvokerAll(mod)
```

这两个调用的顺序属于隐含约定。遗漏其中一步，模块就可能无法被正常反射调用。把这些步骤集中到框架内部，通常能让模块接入过程更稳定。

业务模块之间也会通过具体方法名连接：

```go
mod.player.BagMod_RegisterBag(&mod.armouryBag)
mod.player.HeroMod_RefreshHeroProp(class, source)
mod.player.TaskMod_TaskHandler(taskType)
```

这些调用表达了真实的业务协作关系，本身没有问题。真正需要警惕的是底层框架也开始依赖 `PlayerObj`、`BagMod`、`HeroMod` 等具体类型。这样一来，底层规范修改时就容易受到具体业务对象的字段、接口或生命周期约束。

## 4. 新设计如何解除这类硬编码

我采用的设计把框架识别模块与模块自身的业务初始化分开。业务模块只需要声明自己的具体类型：

```go
type ArmouryMod struct {
	player *PlayerEnt
	actor.ModObj[*ArmouryMod]
	data   ArmouryData
}
```

框架可以从 `ModObj[*ArmouryMod]` 推导出宿主类型和模块名称，也可以发现宿主类型的公开方法。模块注册时由 `ActorLoader` 统一完成通用初始化：

```go
func NewArmouryMod(player *PlayerEnt) actor.IModule {
	return &ArmouryMod{
		player: player,
	}
}

loader.AddModule(NewArmouryMod(player))
```

业务构造函数保存业务依赖并创建业务状态，`ActorLoader` 和 `ModObj` 负责模块基座、名称推导、宿主绑定以及方法和协议索引。业务模块仍然可以拥有自己的初始化逻辑，只是不用重复维护框架调用顺序。

下面的表格展示早期实现与 `ModObj[T]` 方案的差异：

| 对比项 | 早期 `ArmouryMod` | `ModObj[T]` 方案 |
| --- | --- | --- |
| 宿主传递 | `host interface{}`，运行时断言 | 类型参数 `T` 表达具体宿主 |
| 模块名称 | 调用 `Init(name, host)` 手工传入 | 从宿主类型名推导 |
| 方法注册 | 手工调用 `SetInvokerAll(mod)` | `Init` 自动完成 |
| 框架初始化 | 混在业务模块 `Init` 中 | 由 `ModObj` 和 `AddModule` 负责 |
| 方法调用 | 依赖显式注册流程 | 自动扫描公开方法并缓存 |
| 主要风险 | 漏调用、参数错误、顺序错误 | 泛型类型写错或不当复制模块 |

有些硬编码属于业务规则，应该继续留在业务模块中：

```go
func (mod *ArmouryMod) OnDestroy() {
	mod.player.BagMod_UnregisterBag(&mod.armouryBag)
	mod.player.TaskMod_UnRegisterTaskHander(mod.taskHandler)
}
```

兵器模块必须在销毁时解除自己注册的背包和任务处理器，这属于兵器业务的生命周期规则，不应由通用 `ModObj` 猜测。

我认为更应该被消除的是框架层硬编码，例如基类要求所有业务模块传入某种具体玩家类型，或者要求调用方手工传模块名称和手工触发反射注册。

## 5. 基本写法

一个业务模块的完整结构如下：

```go
package bag

import (
	"actor"
	"actor/cmd/GameSvr/player"
)

type BagMod struct {
	host  *player.PlayerEnt
	actor.ModObj[*BagMod]
	items map[int]int
}

func NewBagMod(host *player.PlayerEnt) actor.IModule {
	return &BagMod{
		host:  host,
		items: make(map[int]int),
	}
}

func (mod *BagMod) AddItem(itemID int, count int) {
	mod.items[itemID] += count
}

func (mod *BagMod) ItemCount(itemID int) int {
	return mod.items[itemID]
}
```

最关键的声明是：

```go
actor.ModObj[*BagMod]
```

它表达了两层关系。`BagMod` 通过结构体嵌入获得 `ModObj` 的通用方法，同时把 `*BagMod` 作为类型参数传给 `ModObj`。后者就是 CRTP 的核心特征。

## 6. 结构体嵌入与继承的区别

Go 中的写法如下：

```go
type BagMod struct {
	actor.ModObj[*BagMod]
}
```

这属于组合。`BagMod` 包含一个 `ModObj[*BagMod]` 字段，因此可以使用嵌入字段的方法，但它不属于传统意义上的 `ModObj` 子类，也不存在虚函数重写关系。

## 7. `ModObj[T]` 做了什么

`ModObj[T]` 可以理解为一个模块运行时基座：

```go
type ModObj[T any] struct {
	name           string
	invokers       map[string]*FuncHandler
	metaMsgHandler map[int]string
	heir           T
	storage        IStorage
	isDirty        bool
}
```

`name` 保存模块名称，`invokers` 保存方法名到反射调用器的映射，`metaMsgHandler` 保存协议号到处理方法名的映射，`heir` 保存外层业务模块的引用。其余字段用于存储和脏状态管理。

## 8. 初始化过程

初始化从创建模块对象开始：

```go
mod := &BagMod{}
```

接着调用 `Init`：

```go
mod.Init()
```

`Init` 会创建方法表和协议处理表，根据 `ModObj[T]` 在宿主结构体中的位置计算宿主对象地址，再通过反射扫描公开方法。初始化是幂等的，重复调用不会重新建立方法表。

`ModObj[T]` 的方法接收者是嵌入字段本身。如果知道 `ModObj[*BagMod]` 在 `BagMod` 中的字段偏移量，就可以计算外层对象地址：

```text
BagMod 地址 = ModObj 地址 - ModObj 字段偏移量
```

框架据此通过 `unsafe.Pointer` 找回 `*BagMod`，并保存到 `heir`。因此类型参数必须写成当前模块自身。

初始化完成后，公开业务方法会被记录到方法表：

```text
"AddItem"   -> BagMod.AddItem
"ItemCount" -> BagMod.ItemCount
```

之后 ActorLoader 就可以通过模块名和方法名调用它们：

```go
results, err := loader.ModInvoke("BagMod", "AddItem", 1001, 2)
```

## 9. 方法自动发现与协议处理

方法登记遵循以下规则：只发现公开方法，跳过框架基础方法，其他公开方法进入普通方法表。一个参数、零个返回值且参数类型已经注册协议号的方法，会被识别为协议处理器。

```go
type AddItemMessage struct {
	ItemID int
	Count  int
}

func init() {
	actor.RegisterProtocol(1001, AddItemMessage{})
}

func (mod *BagMod) OnAddItem(message AddItemMessage) {
	mod.AddItem(message.ItemID, message.Count)
}
```

注册协议类型后，框架可以建立下面的索引：

```text
1001 -> OnAddItem
```

## 10. GameSvr 中的完整关系

游戏服务器中的模块可以使用如下结构：

```go
type PlayerEnt struct {
	*actor.User
}

type BagMod struct {
	host *PlayerEnt
	actor.ModObj[*BagMod]
}

type HeroMod struct {
	host *PlayerEnt
	actor.ModObj[*HeroMod]
}
```

创建玩家时，玩家拥有一个 ActorLoader：

```go
func NewPlayerEnt(name string) *PlayerEnt {
	loader := actor.NewActorLoader(name)
	return &PlayerEnt{
		User: actor.NewUser(loader),
	}
}
```

然后把多个模块注册到同一个 ActorLoader：

```go
player := NewPlayerEnt("player-1001")
player.GetModloader().AddModule(bag.NewBagMod(player))
player.GetModloader().AddModule(hero.NewHeroMod(player))
```

模块共享同一个 Actor 的事件循环，因此模块状态通常由同一条 goroutine 串行访问。

## 11. 对外调用与 Facade

代码生成器会为模块方法生成玩家实体上的包装方法：

```go
func (player *PlayerEnt) BagAddItem(itemID int, count int) {
	_, err := player.GetModloader().ModInvoke(
		"BagMod",
		"AddItem",
		itemID,
		count,
	)
	if err != nil {
		println("BagAddItem error:", err.Error())
	}
}
```

调用方只需要写：

```go
player.BagAddItem(1001, 2)
```

生成的包装方法属于 **Facade（外观模式）**。它把模块名称、方法名称和 ActorLoader 调用细节隐藏起来。

## 12. 和其他模式的区别

我会把这套结构与几种常见概念区分开来。传统继承强调子类与父类的类型关系，并依靠重写父类方法改变行为。`BagMod` 采用的是 Go 的结构体嵌入，它包含 `ModObj[*BagMod]`，但不属于 `ModObj` 的子类。

依赖注入关注对象依赖由外部提供。`ModObj[T]` 的职责是模块能力复用、方法发现和反射调用，`BagMod` 中的 `host` 只是业务依赖，因此我不会把它定义为依赖注入框架。

Go 接口适合表达只关心行为的抽象：

```go
type ItemService interface {
	AddItem(int, int)
}
```

CRTP-like 结构适合表达通用基座与具体宿主之间的类型关系。两种方式可以在同一套系统中并存。

## 13. 优点

在实际协作中，我认为这套方案的价值主要有三点：

- 业务模块不需要重复实现模块名称、方法表和协议索引。
- `ModObj[T]` 集中了模块运行所需的通用逻辑，修改框架行为时通常只需要调整通用基座。
- `ModObj[*BagMod]` 明确表达了宿主类型，ActorLoader 仍然可以根据模块名和方法名完成运行时调度。

## 14. 代价和限制

当然，这套方案也有代价。我在使用时会特别关注以下限制：

- 反射调用需要查表并执行 `reflect.Value.Call`，性能低于直接调用。反射扫描适合放在初始化阶段，运行期间应复用已经建立的方法表。
- `unsafe` 会增加对内存布局和对象生命周期的依赖。
- `ModObj` 的类型参数必须写成当前模块，例如 `ModObj[*BagMod]`。类型写错后，框架无法正确恢复宿主对象。
- 模块初始化后不应复制结构体，否则 `ModObj` 内部保存的宿主引用可能仍然指向旧对象。模块应使用指针构造和注册。
- 方法名是运行时契约。模块名或方法名变化时，调用方、代码生成器和协议路由都可能受到影响。
