package actor

import (
	"sync"
	"testing"
)

type injectTarget struct {
	Name string
	Ent  *Entity
}

type injectNoEntity struct {
	Name string
}

type injectLogger struct {
	Name string
}

type injectMultiTarget struct {
	Ent *Entity
	Log *injectLogger
}

func resetInjectTypeCache() {
	typeCache = sync.Map{}
}

func TestInject_Success(t *testing.T) {
	resetInjectTypeCache()

	m := &injectTarget{Name: "u1"}
	e := &Entity{ID: 101}

	ok := Inject(m, e)
	if !ok {
		t.Fatalf("Inject should return true")
	}
	if m.Ent != e {
		t.Fatalf("entity field should be injected")
	}
	e.ID = 102
	if m.Ent.ID != 102 {
		t.Fatalf("entity field should reflect changes")
	}
}

func TestInject_InvalidInput(t *testing.T) {
	resetInjectTypeCache()
	e := &Entity{ID: 1}

	if Inject(nil, e) {
		t.Fatalf("nil model should return false")
	}

	var nilPtr *injectTarget
	if Inject(nilPtr, e) {
		t.Fatalf("nil pointer should return false")
	}

	nonPtr := injectTarget{}
	if Inject(nonPtr, e) {
		t.Fatalf("non pointer should return false")
	}

	arr := &[1]int{1}
	if Inject(arr, e) {
		t.Fatalf("non struct pointer should return false")
	}
}

func TestInject_NoEntityField_FirstCallFalse(t *testing.T) {
	resetInjectTypeCache()

	m := &injectNoEntity{Name: "x"}
	if Inject(m, &Entity{ID: 1}) {
		t.Fatalf("model without *Entity field should return false")
	}
}

func TestInject_NoEntityField_SecondCallFalse(t *testing.T) {
	resetInjectTypeCache()

	m := &injectNoEntity{Name: "x"}
	if Inject(m, &Entity{ID: 1}) {
		t.Fatalf("first call should return false")
	}
	if Inject(m, &Entity{ID: 2}) {
		t.Fatalf("second call should also return false")
	}
}

func TestInjectUnsafe_Success(t *testing.T) {
	resetInjectTypeCache()

	m := &injectTarget{Name: "unsafe"}
	e := &Entity{ID: 202}

	ok := InjectUnsafe(m, e)
	if !ok {
		t.Fatalf("InjectUnsafe should return true")
	}
	if m.Ent != e {
		t.Fatalf("entity field should be injected")
	}
}

func TestInjectUnsafe_InvalidInput(t *testing.T) {
	resetInjectTypeCache()
	e := &Entity{ID: 1}

	if InjectUnsafe(nil, e) {
		t.Fatalf("nil model should return false")
	}

	var nilPtr *injectTarget
	if InjectUnsafe(nilPtr, e) {
		t.Fatalf("nil pointer should return false")
	}

	if InjectUnsafe(injectTarget{}, e) {
		t.Fatalf("non pointer should return false")
	}

	arr := &[1]int{1}
	if InjectUnsafe(arr, e) {
		t.Fatalf("non struct pointer should return false")
	}
}

func TestInjectRef_MultiTypeBinding(t *testing.T) {
	resetInjectTypeCache()

	m := &injectMultiTarget{}
	e := &Entity{ID: 11}
	l := &injectLogger{Name: "L1"}

	if !InjectRef(m, e) {
		t.Fatalf("InjectRef[Entity] should return true")
	}
	if m.Ent != e {
		t.Fatalf("entity field should be injected")
	}

	if !InjectRef(m, l) {
		t.Fatalf("InjectRef[injectLogger] should return true")
	}
	if m.Log != l {
		t.Fatalf("logger field should be injected")
	}
}

func TestInjectUnsafeRef_MultiTypeBinding(t *testing.T) {
	resetInjectTypeCache()

	m := &injectMultiTarget{}
	e := &Entity{ID: 12}
	l := &injectLogger{Name: "L2"}

	if !InjectUnsafeRef(m, e) {
		t.Fatalf("InjectUnsafeRef[Entity] should return true")
	}
	if m.Ent != e {
		t.Fatalf("entity field should be injected")
	}

	if !InjectUnsafeRef(m, l) {
		t.Fatalf("InjectUnsafeRef[injectLogger] should return true")
	}
	if m.Log != l {
		t.Fatalf("logger field should be injected")
	}
}

func TestInjectRef_NoTargetField(t *testing.T) {
	resetInjectTypeCache()

	m := &injectNoEntity{Name: "x"}
	l := &injectLogger{Name: "L3"}

	if InjectRef(m, l) {
		t.Fatalf("InjectRef should return false when target field does not exist")
	}
	if InjectUnsafeRef(m, l) {
		t.Fatalf("InjectUnsafeRef should return false when target field does not exist")
	}
}

func BenchmarkInject_Cached(b *testing.B) {
	resetInjectTypeCache()

	m := &injectTarget{}
	e := &Entity{ID: 7}
	if !InjectRef(m, e) {
		b.Fatalf("warmup inject failed")
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if !InjectRef(m, e) {
			b.Fatalf("inject failed")
		}
	}
}

func BenchmarkInjectUnsafe_Cached(b *testing.B) {
	resetInjectTypeCache()

	m := &injectTarget{}
	e := &Entity{ID: 8}
	if !InjectUnsafe(m, e) {
		b.Fatalf("warmup inject unsafe failed")
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if !InjectUnsafeRef(m, e) {
			b.Fatalf("inject unsafe failed")
		}
	}
}
