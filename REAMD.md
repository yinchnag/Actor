我要创建一个游戏服务器的Actor框架，但有些需求得按照我的方式来

1.设计一个最基础的`IModule`对象
作用: 游戏服务器中某个模块接口，方便后期对所有模块做统一管理，并通过统一接口调用。
```golang
type IModule interface {
    // 初始化
    Init() 
    // 保存模块数据
    Save() 
    // 加载模块数据
    Load() 
    // 通过函数名调用公共成员函数
    Invoke(string,...any) ([]reflect.Value, error)
    // 帧函数
    Update(int64) 
    // 获取本模块协议ID与函数的绑定关系
    //  外部在响应协议时，根据协议ID获取本模块绑定的函数名
    //  ```golang
    //  methodName := imod.GetMetaHandler(1001)
    //  if methodName == "" {
    //      return
    //  }
    //  imod.Invoke(methodName,msg)
    //  ```
    GetMetaHandler(msg int) string 
    // 本模块数据是否需要存档
    IsDirty() bool
    // 设置本模块藏标记函数
    SetDirty(bool)
}
```
针对`IModule`预先实现一个`ModObj`对象,方便后面的组合继承
```golang
// IStorage 对象 
type IStorage interface{
    // 存储对象初始化
    Init()
    // 存储对象存档
    // Save和Load存档函数没有返回值，是因为这里底层对存储是否成功有做处理
    // 底层在多次尝试存储失败后，会记录日志
    Save()
    // 存储对象载档
    Load()
}
// 模块反射的公有函数对象
type FuncHandler struct {
    // 所属模块名
	ModName  string 
    // 函数名
	MethodName string 
    // 函数主体反射对象
    ref      reflect.Value 
    // 是否有绑定协议ID，如果没有则为0
	MsgId    int 
    // 所属模块对应的接口
	IMod     IModule 
    // 入参数量
	NumIn    int 
    // 出参数量
    NumOut   int 
}
// ModObj设计需要使用CRTP思路
//  在子类定义上只需要组合继承ModObj即可
//  ```golang
//  type ShopMod struct {
//      ModObj[*ShopMod]
//  }
//  不过在使用前必须先调用 Init()函数才行，否则后续的函数调用可能会因为父类ModObj中的heir未初始化而导致出现空引用的问题
//  ModObj已经实现了`IModule`所有接口，其他模块只需要组合继承即可
//  ```
type ModObj[T any] struct {
	name           string // 通过反射获得的模块名
    // 反射业务逻辑函数，所有的公共成员函数通过反射都放到这里面，不包含ModObj的函数
    // key-函数名，value是一个成员函数的反射后封装对象
	invokers       map[string]*FuncHandler 
    // 存储着协议ID与函数的绑定关系
    // 协议响应函数一定没有返回值
	metaMsgHandler map[int]string
    // 宿主对象，通过指针偏移拿到的
    //  invokers和metaMsgHandler所需要的成员函数数据则是基于 heir 反射出来的
    heir           T
    // 通过指针偏移拿到 heir 对象下可转换成`IStorage`接口的成员变量，后续存档载档都直接操作这个引用
    storage         IStorage
    // 本模块存档藏标记
    // 可通过调用 SetDirty(true)设置后，会在Update帧函数中下一次帧逻辑中调用Save存档
    isDirty        bool
}

func (that *ModObj[T]) Init() {
	that.invokers = map[string]reflect.Value{}
	that.metaMsgHandler = map[int]*contracts.MsgHandler{}
	that.setInvokerAll()
}

// 模块数据存档
//  底层实现 storage 的存档逻辑，但需要额外注意接口是否为空的判断
func (that *ModObj[T]) Save()

// 模块数据载档
//  底层实现 storage 的载档逻辑，但需要额外注意接口是否为空的判断
func (that *ModObj[T]) Load()

// 通过函数名发起函数调用
func (that *ModObj[T]) Invoke(string,...any) ([]reflect.Value, error)

// 根据游戏服务器逻辑帧执行
// 底层实现 基础的存档判断逻辑
//  ```golang
//  if that.IsDirty() {
//      that.storage.Save()
//  }
func (that *ModObj[T]) Update(dt int64)

// 通过协议ID获得与之绑定的函数名
func (that *ModObj[T]) GetMetaHandler(msg int) string

func (that *ModObj[T]) IsDirty() bool {
    return that.isDirty
}

func (that *ModObj[T]) SetDirty(dirty bool) {
    that.isDirty = dirty
}

// 通过指针偏移为heir赋值，并通过反射为`ModObj[T]`的成员变量赋值
// 这个函数执行时需要将heir的所有公共函数反射存储到 invokers对象中，并且需要分析入参与返参，来判断是否为协议响应函数
// 简单说明协议响应函数是只接受一个参数，这个参数类型的类型名还必须是在协议注册表中的，且没有返回值。
func (that *ModObj[T]) setInvokerAll()

// 返回指定函数的返回值数量
func (that *ModObj[T]) GetNumOut(methodName string) int

// 返回指定函数入参数量
func (that *ModObj[T]) GetNumIn(methodName string) int
```
2.需要实现`ITask`接口
这个接口的实例就是协程之间相互调用的对象
```golang
type ITask interface {
    // 获得当前任务发起的协程ID
	GetGoroutineID() uint64
	// 获得目标协程ID
	GetTargetID() uint64
    // 获得当前任务状态
	GetStatus() int32
	// 获得需要调用的模块名称
	GetModName() string
	// 获得需要执行的函数名称
	GetMethodName() string
    // 获得函数入参
	GetArgs() []any
    // 获得返回值
	GetResults() []reflect.Value
}
// 这里有实现一个`ITask`接口的对象
type ChanTask struct {
	Id          int64
	SourceGID   uint64          // 来源协程
	TargetGID   uint64          // 目标协程
	ModName     string          // 模块名
	MethodName  string          // 函数名称
	Args        []any           // 入参
	Results     []reflect.Value // 出参
	Err         error           // 错误
	Status      int32           // 状态 0 被释放，1 正在使用中 2 调用完成
	ctxTimeOut  chan struct{}
}
// 获得当前任务发起的协程ID
func (that *ChanTask)GetGoroutineID() uint64{
    return that.SourceGID
}
// 获得目标协程ID
func (that *ChanTask)GetTargetID() uint64{
    return that.TargetGID
}
// 获得当前任务状态
func (that *ChanTask)GetStatus() int32{
    return that.Status
}
// 获得需要调用的模块名称
func (that *ChanTask)GetModName() string{
    return that.ModName
}
// 获得需要执行的函数名称
func (that *ChanTask)GetMethodName() string{
    return that.MethodName
}
// 获得函数入参
func (that *ChanTask)GetArgs() []any{
    return that.Args
}
// 获得返回值
func (that *ChanTask)GetResults() []reflect.Value{
    return that.Results
}
// 设置超时时间
func (that *ChanTask) WithTimeout(timeout time.Duration)*ChanTask
// 等待任务执行结束，如果使用`WithTimeout`设置过超时时间，则时间到了自动调用cancel不再等待。
func (that *ChanTask) Await()
// 取消等待，调用协程不再阻塞
func (that *ChanTask)cancel(){
    that.ctxTimeOut<-struct{}{}
}
```

3.需要实现一个`IGoroutine`接口
```golang
type IGoroutine interface {
    // 是否关闭
	IsClose() bool                 
    // 获取协程ID
	GetGoroutineID() uint64        
    // 设置协程ID,这里传递一个协程类型名称进去，比如`网络协程`,`用户协程`,至于协程ID，需要
    // 在函数内部 依赖 `runtime.Stack`自己查询，虽说`runtime.Stack`非常消耗性能，但这个操作只有在创建
    // 协程对象时才会执行，
	SetGoroutineID(string)         
    // 运行更新循环
    // 这里面有个1秒执行一次的定时器，另外使用`select`还`case`了一个`chan`,这个`chan`在接受别的协程塞入的 `ChanTask`任务对象
	RunUpdateLoop(*sync.WaitGroup) 
    // 获得任务通道
    GetTaskChan() chan<- ITask 
}
```
4.需要实现一个`IUser`接口
```golang
type IUser interface {
    // 发送协议
    SendMessage(IProtocol)
    // 获得模块加载器
    GetModloader() IModloader
}
```
5.需要实现一个`IProtocol`接口
```golang
type IProtocol interface {
    // 获得协议号
    GetMessageID() int
    // 获得google协议
    GetMessage() proto.IMessage
    // 获得用户对象
    GetUser() IUser
}
```
6.我需要一个所有Actor对象都实现`IModLoader`接口
每一个`IModloader`实例都是一个协程对象，协程之间的调用通过`chan`传递`ITask`来调用，如果调用
函数有返回值，则调用协程需要进入阻塞状态，等待被调用协程执行结束后调用协程才可以离开阻塞状态。
```golang
type IModLoader interface {
    IGoroutine
    // 初始化
	Init() 
    // 通过模块名称获得一个模块
    GetModule(string) IModule 
    // 增加一个模块
    AddModule(IModule) 
    // 调用某个模块的某个函数，但这里不直接调用，需要检查是否跨协程了，如果是则将调用信息封装成`ChanTask`对象，再将这个对象通过`Chan`传递到被调用者协程
    // 但如果没有跨协程，则直接调用，不用封装后通过`Chan`传递了。
    ModInvoke(string,string,...any) ([]reflect.Value, error)
    // 通过协议ID匹配，触发所有Module中绑定这个协议ID的函数。这里需要注意的是消息来自网络协程，所以这里一定不能直接调用，只能通过ModInvoke函数将调用信息封装成
    // `ChanTask`对象，通过`Chan`传递到正确协程执行
    OnMessageHandler(IProtocol)
}
```

# 代码框架分析
每个`User`实例都可转换成`IModLoader`，`IGoroutine`和`IUser`接口对象。如果
