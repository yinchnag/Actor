package actor

import (
	"reflect"
	"sync"
)

var (
	protocolRegistryMu sync.RWMutex
	protocolTypeByID   = map[int]reflect.Type{}
	protocolIDByType   = map[reflect.Type]int{}
)

func normalizeType(t reflect.Type) reflect.Type {
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Ptr {
		return t.Elem()
	}
	return t
}

// RegisterProtocol binds message ID to a message sample type.
func RegisterProtocol(msgID int, sample any) {
	t := normalizeType(reflect.TypeOf(sample))
	if t == nil {
		return
	}

	protocolRegistryMu.Lock()
	defer protocolRegistryMu.Unlock()
	protocolTypeByID[msgID] = t
	protocolIDByType[t] = msgID
}

func MessageIDByType(t reflect.Type) (int, bool) {
	n := normalizeType(t)
	if n == nil {
		return 0, false
	}

	protocolRegistryMu.RLock()
	defer protocolRegistryMu.RUnlock()
	id, ok := protocolIDByType[n]
	return id, ok
}
