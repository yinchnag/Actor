package actor

import "sync"

type IGoroutine interface {
	IsClose() bool
	GetGoroutineID() uint64
	SetGoroutineID(string)
	RunUpdateLoop(*sync.WaitGroup)
	GetTaskChan() chan<- ITask
}
