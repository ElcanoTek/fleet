package safe

import "testing"

func TestEmitPanic_CountsAndFansOut(t *testing.T) {
	const loc = "unit.emit-test"

	var hookName string
	var writerLoc, writerMsg string
	prevSentry, prevWriter := SentryHook, PanicEventWriter
	SentryHook = func(name string, _ any, _ []byte) { hookName = name }
	PanicEventWriter = func(location, message string, _ []byte) { writerLoc, writerMsg = location, message }
	t.Cleanup(func() { SentryHook, PanicEventWriter = prevSentry, prevWriter })

	before := PanicCounts()[loc]
	EmitPanic(loc, "kaboom", []byte("stack"))

	if got := PanicCounts()[loc]; got != before+1 {
		t.Errorf("PanicCounts[%s] = %d, want %d", loc, got, before+1)
	}
	if hookName != loc {
		t.Errorf("SentryHook got %q, want %q", hookName, loc)
	}
	if writerLoc != loc || writerMsg != "kaboom" {
		t.Errorf("PanicEventWriter got (%q,%q), want (%q,kaboom)", writerLoc, writerMsg, loc)
	}
}

func TestRecover_FansOutToWriter(t *testing.T) {
	var gotMsg string
	prev := PanicEventWriter
	PanicEventWriter = func(_, message string, _ []byte) { gotMsg = message }
	t.Cleanup(func() { PanicEventWriter = prev })

	func() {
		defer Recover("unit.recover-writer", nil)
		panic("from-recover")
	}()
	if gotMsg != "from-recover" {
		t.Errorf("writer message = %q, want from-recover", gotMsg)
	}
}

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
