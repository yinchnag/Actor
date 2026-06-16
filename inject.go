package actor

import (
	"reflect"
	"sync"
	"unsafe"
)

type Entity struct {
	ID int64
}

type fieldMeta struct {
	offset uintptr
}

type cacheKey struct {
	modelType reflect.Type
	refType   reflect.Type
}

var typeCache sync.Map // map[cacheKey]*fieldMeta

func loadFieldMeta(key cacheKey) (*fieldMeta, bool) {
	v, ok := typeCache.Load(key)
	if !ok {
		return nil, false
	}

	if v == nil {
		return nil, true
	}

	meta, ok := v.(*fieldMeta)
	if !ok {
		return nil, false
	}

	return meta, true
}

func validateModelStructPtr(model any) (reflect.Value, bool) {
	v := reflect.ValueOf(model)
	if !v.IsValid() || v.Kind() != reflect.Ptr || v.IsNil() {
		return reflect.Value{}, false
	}

	elem := v.Elem()
	if elem.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}

	return elem, true
}

func refPointerType[T any]() reflect.Type {
	var p *T
	return reflect.TypeOf(p)
}

func Inject(model any, e *Entity) bool {
	return InjectRef(model, e)
}

func InjectRef[T any](model any, ref *T) bool {
	elem, ok := validateModelStructPtr(model)
	if !ok {
		return false
	}

	t := elem.Type()
	key := cacheKey{modelType: t, refType: refPointerType[T]()}

	// ====== fast path（缓存）======
	if meta, ok := loadFieldMeta(key); ok {
		return injectFast(elem, meta, ref)
	}

	// ====== slow path（反射扫描）======
	meta := scanType(t, key.refType)
	typeCache.Store(key, meta)

	return injectFast(elem, meta, ref)
}

func scanType(t reflect.Type, refType reflect.Type) *fieldMeta {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		if f.Type == refType {
			return &fieldMeta{
				offset: f.Offset,
			}
		}
	}
	return nil
}

func injectFast[T any](v reflect.Value, meta *fieldMeta, ref *T) bool {
	if meta == nil {
		return false
	}

	ptr := unsafe.Pointer(v.UnsafeAddr())
	fieldPtr := (**T)(unsafe.Pointer(uintptr(ptr) + meta.offset))

	*fieldPtr = ref
	return true
}
func InjectUnsafe(model any, e *Entity) bool {
	return InjectUnsafeRef(model, e)
}

func InjectUnsafeRef[T any](model any, ref *T) bool {
	elem, ok := validateModelStructPtr(model)
	if !ok {
		return false
	}

	t := elem.Type()
	key := cacheKey{modelType: t, refType: refPointerType[T]()}

	if meta, ok := loadFieldMeta(key); ok {
		return injectRaw(unsafe.Pointer(elem.UnsafeAddr()), meta, ref)
	}

	meta := scanType(t, key.refType)
	typeCache.Store(key, meta)

	return injectRaw(unsafe.Pointer(elem.UnsafeAddr()), meta, ref)
}

func injectRaw[T any](ptr unsafe.Pointer, meta *fieldMeta, ref *T) bool {
	if meta == nil {
		return false
	}

	fieldPtr := (**T)(unsafe.Pointer(uintptr(ptr) + meta.offset))
	*fieldPtr = ref

	return true
}
