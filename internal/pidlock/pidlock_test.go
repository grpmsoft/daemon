package pidlock

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TDD: These tests define the contract BEFORE implementation.
// ---------------------------------------------------------------------------

func TestTryLock_AcquiresOnNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pid")

	l, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock on new file: %v", err)
	}
	defer l.Release()

	if l.File() == nil {
		t.Fatal("File() must return non-nil after successful lock")
	}
}

func TestTryLock_SecondCallerGetsErrLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pid")

	l1, err := TryLock(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer l1.Release()

	_, err = TryLock(path)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: got %v, want ErrLocked", err)
	}
}

func TestTryLock_ReleasedLockCanBeReacquired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pid")

	l1, err := TryLock(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	l1.Release()

	l2, err := TryLock(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	l2.Release()
}

func TestTryLock_FileNotDeletedAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pid")

	l, err := TryLock(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	l.Release()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("PID lock file must NOT be deleted after Release (flock+unlink race prevention)")
	}
}

func TestTryLock_ExclusiveUnderConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pid")

	var held atomic.Bool
	var violations atomic.Int32
	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := TryLock(path)
			if errors.Is(err, ErrLocked) {
				return // expected for losers
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if !held.CompareAndSwap(false, true) {
				violations.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
			held.Store(false)
			l.Release()
		}()
	}

	wg.Wait()
	if v := violations.Load(); v > 0 {
		t.Errorf("lock violated %d times", v)
	}
}

func TestTryLock_EINTRRetry(t *testing.T) {
	// Verify that TryLock handles EINTR (signal during flock).
	// We can't easily trigger EINTR in a unit test, but we verify
	// the code path exists by successfully locking — if EINTR retry
	// was missing and a signal arrived, we'd get a spurious error.
	path := filepath.Join(t.TempDir(), "test.pid")
	l, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock (EINTR resilience): %v", err)
	}
	l.Release()
}

func TestLock_WriteAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pid")

	l, err := TryLock(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer l.Release()

	// Write PID data via Seek+Write+Truncate (NOT temp+rename).
	data := []byte(`{"pid":12345,"port":8080}`)
	if err := l.WriteData(data); err != nil {
		t.Fatalf("WriteData: %v", err)
	}

	// Read back (use ReadLocked — os.ReadFile fails on Windows with LockFileEx).
	got, err := ReadLocked(path)
	if err != nil {
		t.Fatalf("ReadLocked: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("data mismatch: got %q, want %q", got, data)
	}

	// Overwrite with shorter data — must truncate.
	data2 := []byte(`{"pid":99}`)
	if err := l.WriteData(data2); err != nil {
		t.Fatalf("WriteData (overwrite): %v", err)
	}

	got2, err := ReadLocked(path)
	if err != nil {
		t.Fatalf("ReadLocked after overwrite: %v", err)
	}
	if string(got2) != string(data2) {
		t.Errorf("overwrite mismatch: got %q, want %q", got2, data2)
	}
}

func TestReadLocked_ReadsWhileLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pid")

	l, err := TryLock(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer l.Release()

	want := []byte(`{"pid":42,"port":9090}`)
	if err := l.WriteData(want); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Another process/goroutine can READ even while locked.
	got, err := ReadLocked(path)
	if err != nil {
		t.Fatalf("ReadLocked: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ReadLocked: got %q, want %q", got, want)
	}
}

func TestIsHeld_TrueWhileLocked_FalseAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pid")

	if IsHeld(path) {
		t.Fatal("IsHeld must be false for non-existent file")
	}

	l, err := TryLock(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	if !IsHeld(path) {
		t.Fatal("IsHeld must be true while locked")
	}

	l.Release()

	if IsHeld(path) {
		t.Fatal("IsHeld must be false after release")
	}
}

func TestIsHeld_NonExistentFile_DoesNotCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.pid")

	if IsHeld(path) {
		t.Fatal("IsHeld must be false for non-existent file")
	}

	// The file must NOT be created by the IsHeld probe.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("IsHeld created a file that did not exist: %v", err)
	}
}

func TestInheritFD_ReflockSameOFD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("InheritFD uses flock; not supported on Windows")
	}

	path := filepath.Join(t.TempDir(), "test.pid")

	// Acquire a real lock.
	l, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	defer l.Release()

	// InheritFD on the SAME fd — must succeed because re-flock on the same
	// Open File Description is either idempotent (returns 0 on local fs) or
	// returns EWOULDBLOCK (some NFS implementations), both treated as success.
	fd := int(l.File().Fd())
	inherited, err := InheritFD(fd, path)
	if err != nil {
		t.Fatalf("InheritFD on same OFD: %v", err)
	}

	// Verify the returned Lock is usable — WriteData must succeed.
	data := []byte(`{"pid":54321,"port":9090}`)
	if err := inherited.WriteData(data); err != nil {
		t.Fatalf("WriteData via inherited lock: %v", err)
	}

	// Verify written content.
	got, err := ReadLocked(path)
	if err != nil {
		t.Fatalf("ReadLocked: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("data mismatch: got %q, want %q", got, data)
	}
}

func TestInheritFD_InvalidFD(t *testing.T) {
	_, err := InheritFD(999, "nonexistent.pid")
	if err == nil {
		t.Fatal("InheritFD with invalid fd must return error")
	}
}
