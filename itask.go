package actor

import "reflect"

type ITask interface {
	GetGoroutineID() uint64
	GetTargetID() uint64
	GetStatus() int32
	GetModName() string
	GetMethodName() string
	GetArgs() []any
	GetResults() []reflect.Value
}
