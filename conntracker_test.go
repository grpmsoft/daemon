package daemon

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Tests: ConnTracker
// ---------------------------------------------------------------------------

func TestConnTracker_InitialActiveIsZero(t *testing.T) {
	ct := NewConnTracker()
	if got := ct.Active(); got != 0 {
		t.Errorf("initial Active() = %d, want 0", got)
	}
}

func TestConnTracker_ConnectIncrements(t *testing.T) {
	ct := NewConnTracker()
	ct.Connect()
	if got := ct.Active(); got != 1 {
		t.Errorf("Active() after 1 Connect = %d, want 1", got)
	}
	ct.Connect()
	if got := ct.Active(); got != 2 {
		t.Errorf("Active() after 2 Connects = %d, want 2", got)
	}
}

func TestConnTracker_DisconnectDecrements(t *testing.T) {
	ct := NewConnTracker()
	ct.Connect()
	ct.Connect()
	ct.Disconnect()
	if got := ct.Active(); got != 1 {
		t.Errorf("Active() after 2 Connect + 1 Disconnect = %d, want 1", got)
	}
}

func TestConnTracker_DisconnectToZero_SignalsIdleCh(t *testing.T) {
	ct := NewConnTracker()
	ct.Connect()
	ct.Disconnect()

	select {
	case <-ct.idleCh:
		// expected: idle signal received
	case <-time.After(time.Second):
		t.Fatal("idleCh must be signaled when connections drop to zero")
	}
}

func TestConnTracker_Connect_SignalsResetCh(t *testing.T) {
	ct := NewConnTracker()
	ct.Connect()

	select {
	case <-ct.resetCh:
		// expected: reset signal received
	case <-time.After(time.Second):
		t.Fatal("resetCh must be signaled on Connect")
	}
}

func TestConnTracker_DisconnectAboveZero_NoIdleSignal(t *testing.T) {
	ct := NewConnTracker()
	ct.Connect()
	ct.Connect()
	ct.Disconnect() // count goes from 2 to 1, not zero

	select {
	case <-ct.idleCh:
		t.Fatal("idleCh must NOT be signaled when count is still > 0")
	case <-time.After(50 * time.Millisecond):
		// expected: no signal
	}
}

func TestConnTracker_MultipleDisconnectsToZero_NonBlocking(t *testing.T) {
	ct := NewConnTracker()
	ct.Connect()
	ct.Disconnect() // first signal fills the buffered channel
	ct.Connect()
	ct.Disconnect() // second signal must not block (non-blocking send)

	if got := ct.Active(); got != 0 {
		t.Errorf("Active() = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Tests: waitForIdle
// ---------------------------------------------------------------------------

func TestWaitForIdle_FiresAfterTimeout(t *testing.T) {
	ct := NewConnTracker()
	ctx := context.Background()

	// Simulate: connections are already at zero, signal the idle channel.
	go func() {
		time.Sleep(10 * time.Millisecond)
		ct.idleCh <- struct{}{}
	}()

	done := make(chan error, 1)
	go func() {
		done <- waitForIdle(ctx, ct, 50*time.Millisecond)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("waitForIdle should return nil when timeout fires, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForIdle did not return within 3 seconds")
	}
}

func TestWaitForIdle_ResetsOnNewConnection(t *testing.T) {
	ct := NewConnTracker()
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- waitForIdle(ctx, ct, 100*time.Millisecond)
	}()

	// Signal idle (connections at zero).
	time.Sleep(10 * time.Millisecond)
	ct.idleCh <- struct{}{}

	// Before the timer fires (100ms), signal a new connection to reset.
	time.Sleep(30 * time.Millisecond)
	ct.resetCh <- struct{}{}

	// Now let it actually idle out: signal idle again and let the timer expire.
	time.Sleep(10 * time.Millisecond)
	ct.idleCh <- struct{}{}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("waitForIdle should return nil after reset + re-idle, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForIdle did not return within 3 seconds")
	}
}

func TestWaitForIdle_CancelledContext_ReturnsError(t *testing.T) {
	ct := NewConnTracker()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- waitForIdle(ctx, ct, time.Minute) // long timeout
	}()

	// Cancel immediately.
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("waitForIdle must return an error when context is cancelled")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForIdle did not return within 3 seconds")
	}
}

func TestWaitForIdle_CancelDuringTimer_ReturnsError(t *testing.T) {
	ct := NewConnTracker()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- waitForIdle(ctx, ct, 5*time.Second) // long timer
	}()

	// Signal idle to start the timer.
	time.Sleep(10 * time.Millisecond)
	ct.idleCh <- struct{}{}

	// Cancel while the timer is running (before the 5s timeout fires).
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("waitForIdle must return an error when context is cancelled during timer")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForIdle did not return within 3 seconds")
	}
}
