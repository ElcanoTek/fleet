package safe

import "testing"

func TestRecover_RecoversAndRunsOnPanic(t *testing.T) {
	called := false
	var recovered any
	func() {
		defer Recover("unit", func(v any) {
			called = true
			recovered = v
		})
		panic("boom")
	}()
	if !called {
		t.Fatal("onPanic was not invoked after a panic")
	}
	if recovered != "boom" {
		t.Fatalf("onPanic received %v, want \"boom\"", recovered)
	}
}

func TestGo_RecoversPanic(_ *testing.T) {
	done := make(chan struct{})
	// If Go did not recover, this panic would crash the whole test process.
	Go("unit", func() {
		defer close(done)
		panic("boom")
	})
	<-done
}
