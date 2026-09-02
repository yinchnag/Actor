package comm

// AccountSnap 是账号的跨模块快照。
//
// 名字里的 Snap 是规范要求：本包里的 struct 一律以 Snap 结尾，用来标记
// "这是可以跨模块传递的值"。反过来说，不带 Snap 的类型就不该出现在
// 模块与模块之间——那类型属于某个模块自己，别人不该认识它。
//
// 它是**值快照**，不是存储对象。databases.Account 内嵌了 Norm 的 TableSchema，
// 带着 unsafe 指针和一次性初始化状态，一旦离开 databases 包被随手拷贝，
// selfPtr 就会指向旧对象。让那个类型止步于 databases，跨层只传这里的值。
type AccountSnap struct {
	UID          string // 账号唯一 ID，本项目里就是手机号
	PasswordHash string // bcrypt 哈希
	RegisterDate int64  // 注册时间（毫秒时间戳）
	LoginDate    int64  // 最近登录时间（毫秒时间戳）
}
