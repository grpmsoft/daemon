package daemon

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestGroup_NoActors_ReturnsNil(t *testing.T) {
	var g Group
	err := g.Run()
	if err != nil {
		t.Errorf("empty Group.Run() should return nil, got: %v", err)
	}
}

func TestGroup_SingleActor_ReturnsItsError(t *testing.T) {
	wantErr := errors.New("actor failed")
	var g Group
	g.Add(
		func() error { return wantErr },
		func(error) {},
	)

	err := g.Run()
	if !errors.Is(err, wantErr) {
		t.Errorf("got %v, want %v", err, wantErr)
	}
}

func TestGroup_SingleActor_NilError(t *testing.T) {
	var g Group
	g.Add(
		func() error { return nil },
		func(error) {},
	)

	err := g.Run()
	if err != nil {
		t.Errorf("got %v, want nil", err)
	}
}

func TestGroup_TwoActors_FirstReturnTriggersInterrupt(t *testing.T) {
	actorErr := errors.New("actor-1 done")
	var interrupted atomic.Bool
	stopCh := make(chan struct{})

	var g Group

	// Actor 1: returns immediately with an error.
	g.Add(
		func() error { return actorErr },
		func(error) {},
	)

	// Actor 2: blocks until interrupted.
	g.Add(
		func() error {
			<-stopCh
			return nil
		},
		func(error) {
			interrupted.Store(true)
			close(stopCh)
		},
	)

	err := g.Run()
	if !errors.Is(err, actorErr) {
		t.Errorf("got %v, want %v", err, actorErr)
	}
	if !interrupted.Load() {
		t.Error("actor 2 must be interrupted when actor 1 returns")
	}
}

func TestGroup_InterruptReceivesFirstError(t *testing.T) {
	wantErr := errors.New("trigger error")
	var gotInterruptErr atomic.Value
	stopCh := make(chan struct{})

	var g Group

	g.Add(
		func() error { return wantErr },
		func(error) {},
	)
	g.Add(
		func() error {
			<-stopCh
			return nil
		},
		func(err error) {
			gotInterruptErr.Store(err)
			close(stopCh)
		},
	)

	_ = g.Run()

	got, ok := gotInterruptErr.Load().(error)
	if !ok {
		t.Fatal("interrupt was not called with an error")
	}
	if !errors.Is(got, wantErr) {
		t.Errorf("interrupt received %v, want %v", got, wantErr)
	}
}

func TestGroup_ThreeActors_AllInterrupted(t *testing.T) {
	var count atomic.Int32
	stops := [3]chan struct{}{
		make(chan struct{}),
		make(chan struct{}),
		make(chan struct{}),
	}

	var g Group

	// Actor 0: returns after a short delay.
	g.Add(
		func() error {
			time.Sleep(10 * time.Millisecond)
			return nil
		},
		func(error) {},
	)

	// Actors 1 and 2: block until interrupted.
	for i := 1; i <= 2; i++ {
		idx := i
		g.Add(
			func() error {
				<-stops[idx]
				return nil
			},
			func(error) {
				count.Add(1)
				close(stops[idx])
			},
		)
	}

	err := g.Run()
	if err != nil {
		t.Errorf("got %v, want nil", err)
	}
	if got := count.Load(); got != 2 {
		t.Errorf("expected 2 actors interrupted, got %d", got)
	}
}

func TestGroup_Run_ReturnsFirstActorError_NotLast(t *testing.T) {
	firstErr := errors.New("first")
	secondErr := errors.New("second")

	stopCh := make(chan struct{})

	var g Group

	g.Add(
		func() error { return firstErr },
		func(error) {},
	)
	g.Add(
		func() error {
			<-stopCh
			return secondErr
		},
		func(error) {
			close(stopCh)
		},
	)

	err := g.Run()
	if !errors.Is(err, firstErr) {
		t.Errorf("Run must return the FIRST error, got %v, want %v", err, firstErr)
	}
}

func TestGroup_Run_CompletesWithinTimeout(t *testing.T) {
	stopCh := make(chan struct{})

	var g Group
	g.Add(
		func() error {
			time.Sleep(10 * time.Millisecond)
			return nil
		},
		func(error) {},
	)
	g.Add(
		func() error {
			<-stopCh
			return nil
		},
		func(error) {
			close(stopCh)
		},
	)

	done := make(chan struct{})
	go func() {
		_ = g.Run()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(5 * time.Second):
		t.Fatal("Group.Run() did not complete within 5 seconds")
	}
}
