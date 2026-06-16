package actor

import "reflect"

// FuncHandler stores reflected method metadata.
type FuncHandler struct {
	ModName    string
	MethodName string
	Ref        reflect.Value
	MsgID      int
	IMod       IModule
	NumIn      int
	NumOut     int
}
