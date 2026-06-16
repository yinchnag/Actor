package actor

import "reflect"

type IModLoader interface {
	IGoroutine
	Init()
	GetModule(string) IModule
	AddModule(IModule)
	ModInvoke(string, string, ...any) ([]reflect.Value, error)
	OnMessageHandler(IProtocol)
}
