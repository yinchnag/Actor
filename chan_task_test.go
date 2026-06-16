package actor

import (
	"reflect"
	"testing"
	"time"
)

func TestChanTaskCompleteAndAwait(t *testing.T) {
	task := NewChanTask(1, 2, "EchoMod", "Add", 1, 2)

	go func() {
		time.Sleep(20 * time.Millisecond)
		task.complete([]reflect.Value{reflect.ValueOf(3)}, nil)
	}()

	task.Await()
	if task.GetStatus() != 2 {
		t.Fatalf("expected status=2")
	}
	if len(task.GetResults()) != 1 || int(task.GetResults()[0].Int()) != 3 {
		t.Fatalf("unexpected results")
	}
}

func TestChanTaskTimeout(t *testing.T) {
	task := NewChanTask(1, 2, "EchoMod", "Slow")
	start := time.Now()
	task.WithTimeout(30 * time.Millisecond).Await()

	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("timeout await took too long")
	}
}
