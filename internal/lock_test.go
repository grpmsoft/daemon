package internal

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }

func TestLock_ExclusiveAccess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	var held atomic.Bool
	var violations atomic.Int32
	var wg sync.WaitGroup

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			f, err := Lock(lockPath)
			if err != nil {
				t.Errorf("Lock: %v", err)
				return
			}

			// If another goroutine already holds the lock, that's a violation.
			if !held.CompareAndSwap(false, true) {
				violations.Add(1)
			}

			time.Sleep(10 * time.Millisecond)
			held.Store(false)

			Unlock(f)
		}()
	}

	wg.Wait()

	if v := violations.Load(); v > 0 {
		t.Errorf("lock violated %d times: two goroutines held the lock simultaneously", v)
	}
}

func TestLock_FileNotDeleted(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	f, err := Lock(lockPath)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	Unlock(f)

	// V1 regression: lock file must NOT be deleted after Unlock.
	if _, err := statFile(lockPath); err != nil {
		t.Errorf("lock file deleted after Unlock — V1 regression: %v", err)
	}
}

func TestLock_SecondAcquireAfterUnlock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	f1, err := Lock(lockPath)
	if err != nil {
		t.Fatalf("Lock 1: %v", err)
	}
	Unlock(f1)

	f2, err := Lock(lockPath)
	if err != nil {
		t.Fatalf("Lock 2 after Unlock: %v", err)
	}
	Unlock(f2)
}
