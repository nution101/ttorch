package livestate

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// readWithin runs ReadRecord and fails the test if it has not returned within d. A record
// path the reader blocks on would otherwise hang the watcher's sweep for every worker.
func readWithin(t *testing.T, path string, d time.Duration) (Record, bool) {
	t.Helper()
	type result struct {
		r  Record
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		r, ok := ReadRecord(path, "t1")
		done <- result{r, ok}
	}()
	select {
	case res := <-done:
		return res.r, res.ok
	case <-time.After(d):
		t.Fatalf("ReadRecord(%s) did not return within %v", path, d)
		return Record{}, false
	}
}

// allocDuring reports how many bytes f allocated on the heap.
func allocDuring(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// mkfifo creates a FIFO at path. Cleanup opens and closes its write end, which releases a
// reader still blocked opening it, so a failing run does not leave a goroutine parked.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
}

// largeFile creates a sparse file of n bytes at path.
func largeFile(t *testing.T, path string, n int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(n); err != nil {
		t.Fatal(err)
	}
}

const bigFile = 64 << 20

// TestReadRecord_FIFOReturnsAtOnce: a FIFO at the record path, with nothing writing to it, is
// no hook signal and does not block the reader.
func TestReadRecord_FIFOReturnsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.json")
	mkfifo(t, path)
	if r, ok := readWithin(t, path, 5*time.Second); ok {
		t.Fatalf("a FIFO read as a record: %+v", r)
	}
}

// TestReadRecord_FIFOWithAnIdleWriterReturnsAtOnce: a FIFO some process holds open for
// writing but never writes to is refused before it is read, so the reader cannot park on it.
//
// This is the case ReadRecord's regular-file check is for, and it only goes red without that
// check on Linux. There Go polls a FIFO through epoll, so a read with no data parks the
// goroutine. On darwin Go does not use kqueue for FIFOs (go.dev/issue/24164) and the read
// returns EAGAIN instead, which O_NONBLOCK alone already turns into no signal.
func TestReadRecord_FIFOWithAnIdleWriterReturnsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.json")
	mkfifo(t, path)
	w, err := os.OpenFile(path, os.O_RDWR, 0) // O_RDWR opens a FIFO without waiting for a peer
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if r, ok := readWithin(t, path, 5*time.Second); ok {
		t.Fatalf("a FIFO read as a record: %+v", r)
	}
}

// TestReadRecord_SymlinkIsNoSignal: the record is never read through a symlink, even one to a
// valid record, and one to a large file costs no more than a failed open.
func TestReadRecord_SymlinkIsNoSignal(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "elsewhere.json")
	if err := WriteRecord(valid, rec(TurnStarted, t0)); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "hook.json")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if r, ok := readWithin(t, link, 5*time.Second); ok {
		t.Fatalf("a symlink to a valid record read as a record: %+v", r)
	}

	big := filepath.Join(dir, "big")
	largeFile(t, big, bigFile)
	bigLink := filepath.Join(dir, "hook-big.json")
	if err := os.Symlink(big, bigLink); err != nil {
		t.Fatal(err)
	}
	var ok bool
	if n := allocDuring(func() { _, ok = readWithin(t, bigLink, 5*time.Second) }); n > 1<<20 {
		t.Errorf("reading a symlink to a %d MiB file allocated %d bytes", bigFile>>20, n)
	}
	if ok {
		t.Fatal("a symlink to a large file read as a record")
	}
}

// TestReadRecord_OversizeReadsOnlyTheCap: an oversize regular file is no hook signal, and the
// reader stops one byte past the cap instead of loading the file.
func TestReadRecord_OversizeReadsOnlyTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook.json")
	largeFile(t, path, bigFile)
	var ok bool
	if n := allocDuring(func() { _, ok = readWithin(t, path, 5*time.Second) }); n > 1<<20 {
		t.Errorf("reading a %d MiB record allocated %d bytes", bigFile>>20, n)
	}
	if ok {
		t.Fatal("an oversize file read as a record")
	}
}
