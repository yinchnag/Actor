package actor

import (
	"errors"
	"fmt"
	"reflect"
	"unsafe"
)

type ModObj[T any] struct {
	name           string
	invokers       map[string]*FuncHandler
	metaMsgHandler map[int]string
	heir           T
	storage        IStorage
	isDirty        bool
}

var baseMethodSet = map[string]struct{}{
	"Init":           {},
	"SetStorage":     {},
	"Save":           {},
	"Load":           {},
	"Invoke":         {},
	"Update":         {},
	"GetMetaHandler": {},
	"IsDirty":        {},
	"SetDirty":       {},
	"GetNumOut":      {},
	"GetNumIn":       {},
	"MetaHandlers":   {},
}

func (that *ModObj[T]) SetStorage(storage IStorage) {
	that.storage = storage
}

func (that *ModObj[T]) Init() {
	that.invokers = map[string]*FuncHandler{}
	that.metaMsgHandler = map[int]string{}
	that.bindHeirByOffset()
	that.setInvokerAll()
}

// bindHeirByOffset recovers the embedding object pointer from ModObj field offset.
func (that *ModObj[T]) bindHeirByOffset() {
	var zero T
	heirType := reflect.TypeOf(zero)
	if heirType == nil {
		return
	}

	ownerType := heirType
	isPtr := false
	if ownerType.Kind() == reflect.Ptr {
		isPtr = true
		ownerType = ownerType.Elem()
	}
	if ownerType.Kind() != reflect.Struct {
		return
	}

	modObjType := reflect.TypeOf(ModObj[T]{})
	fieldOffset := uintptr(0)
	found := false
	for i := 0; i < ownerType.NumField(); i++ {
		f := ownerType.Field(i)
		if f.Type == modObjType {
			fieldOffset = f.Offset
			found = true
			break
		}
	}
	if !found {
		return
	}

	ownerPtr := unsafe.Pointer(uintptr(unsafe.Pointer(that)) - fieldOffset)
	if isPtr {
		ownerVal := reflect.NewAt(ownerType, ownerPtr)
		if h, ok := ownerVal.Interface().(T); ok {
			that.heir = h
		}
		return
	}

	ownerVal := reflect.NewAt(ownerType, ownerPtr).Elem()
	if h, ok := ownerVal.Interface().(T); ok {
		that.heir = h
	}
}

func (that *ModObj[T]) Save() {
	if that.storage == nil {
		return
	}
	that.storage.Save()
}

func (that *ModObj[T]) Load() {
	if that.storage == nil {
		return
	}
	that.storage.Load()
}

func (that *ModObj[T]) GameName() string {
	return that.name
}
func (that *ModObj[T]) Invoke(methodName string, args ...any) ([]reflect.Value, error) {
	h, ok := that.invokers[methodName]
	if !ok {
		return nil, fmt.Errorf("method not found: %s", methodName)
	}

	if len(args) != h.NumIn {
		return nil, fmt.Errorf("method %s expects %d args, got %d", methodName, h.NumIn, len(args))
	}

	in := make([]reflect.Value, 0, len(args))
	for i := 0; i < len(args); i++ {
		argVal := reflect.ValueOf(args[i])
		if !argVal.IsValid() {
			return nil, errors.New("invalid nil argument")
		}

		want := h.Ref.Type().In(i)
		if argVal.Type().AssignableTo(want) {
			in = append(in, argVal)
			continue
		}
		if argVal.Type().ConvertibleTo(want) {
			in = append(in, argVal.Convert(want))
			continue
		}

		return nil, fmt.Errorf("argument %d type mismatch: got %s want %s", i, argVal.Type(), want)
	}

	out := h.Ref.Call(in)
	return out, nil
}

func (that *ModObj[T]) Update(dt int64) {
	_ = dt
	if that.IsDirty() {
		that.Save()
		that.SetDirty(false)
	}
}

func (that *ModObj[T]) GetMetaHandler(msg int) string {
	if that.metaMsgHandler == nil {
		return ""
	}
	return that.metaMsgHandler[msg]
}

// MetaHandlers 返回本模块认领的全部「协议 ID → 处理方法」。
//
// 供 ActorLoader 在 AddModule 时一次性建好分发索引：分发路径就不必再逐个模块
// 问 GetMetaHandler，也就不必为了问而先把模块列表拷一份出来。
//
// 返回的是内部 map 本身，调用方只读、不得修改。它在 Init 里建好之后不再变动，
// 所以这样共享是安全的。
func (that *ModObj[T]) MetaHandlers() map[int]string {
	return that.metaMsgHandler
}

func (that *ModObj[T]) IsDirty() bool {
	return that.isDirty
}

func (that *ModObj[T]) SetDirty(dirty bool) {
	that.isDirty = dirty
}

func (that *ModObj[T]) setInvokerAll() {
	hv := reflect.ValueOf(that.heir)
	if !hv.IsValid() {
		return
	}

	ht := hv.Type()
	that.name = indirectTypeName(ht)

	for i := 0; i < ht.NumMethod(); i++ {
		m := ht.Method(i)
		if m.PkgPath != "" {
			continue
		}
		if _, skip := baseMethodSet[m.Name]; skip {
			continue
		}

		mv := hv.MethodByName(m.Name)
		if !mv.IsValid() {
			continue
		}

		numIn := m.Type.NumIn() - 1
		numOut := m.Type.NumOut()
		h := &FuncHandler{
			ModName:    that.name,
			MethodName: m.Name,
			Ref:        mv,
			IMod:       that,
			NumIn:      numIn,
			NumOut:     numOut,
		}
		that.invokers[m.Name] = h

		if numIn == 1 && numOut == 0 {
			if msgID, ok := MessageIDByType(m.Type.In(1)); ok {
				h.MsgID = msgID
				that.metaMsgHandler[msgID] = m.Name
			}
		}
	}
}

func (that *ModObj[T]) GetNumOut(methodName string) int {
	h, ok := that.invokers[methodName]
	if !ok {
		return -1
	}
	return h.NumOut
}

func (that *ModObj[T]) GetNumIn(methodName string) int {
	h, ok := that.invokers[methodName]
	if !ok {
		return -1
	}
	return h.NumIn
}

func indirectTypeName(t reflect.Type) string {
	if t == nil {
		return ""
	}
	if t.Kind() == reflect.Ptr {
		return t.Elem().Name()
	}
	return t.Name()
}
