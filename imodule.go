package actor

import "reflect"

// IModule defines a minimal module contract for actor modules.
type IModule interface {
	Init()
	Save()
	Load()
	GameName() string
	Invoke(string, ...any) ([]reflect.Value, error)
	Update(int64)
	GetMetaHandler(msg int) string
	IsDirty() bool
	SetDirty(bool)
}

// IStorage defines persistence hooks. This stage keeps it optional.
type IStorage interface {
	Init()
	Save()
	Load()
}
