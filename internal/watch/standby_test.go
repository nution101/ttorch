package watch

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDaemon_LogsEachStandbyEpisodeOnce: while another holder has the watch lock the loop
// stands by and says so in the scheduler log once, naming the holder's pid, not once per
// sweep. When the lock frees it logs that it resumed; a later standby is a new episode and is
// logged again.
func TestDaemon_LogsEachStandbyEpisodeOnce(t *testing.T) {
	d, _, fake, _ := newDaemon(t)
	var log bytes.Buffer
	d.Log = &log
	path := d.P.WatchPIDFile()

	hold := func() func() {
		lock, err := acquireFlock(path, "pane:4242:Mon-Jun-29-00:16:32-2026")
		if err != nil {
			t.Fatal(err)
		}
		return func() { releaseFlock(lock, path) }
	}

	release := hold()
	for i := 0; i < 4; i++ {
		res := tick(t, d)
		if !res.Standby || res.HolderPID != os.Getpid() {
			t.Fatalf("sweep %d with the lock held = %+v; want standby naming pid %d", i, res, os.Getpid())
		}
	}
	release()
	tick(t, d)
	tick(t, d)
	release = hold()
	tick(t, d)
	tick(t, d)
	release()

	got := log.String()
	if n := strings.Count(got, "standing by"); n != 2 {
		t.Fatalf("logged %d standby lines over two episodes, want 2:\n%s", n, got)
	}
	if n := strings.Count(got, "resumed"); n != 1 {
		t.Fatalf("logged %d resume lines, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "held by pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("standby line does not name the holder's pid:\n%s", got)
	}
	if fake.typed != 0 {
		t.Fatalf("typed %d wake(s) while standing by", fake.typed)
	}
}

// TestStandbyHolder_SeesALiveHandArmedWatch: with a live process that ps shows as
// `ttorch watch` recorded in the pid file, StandbyHolder reports standby and its pid. The
// loop's own record, a dead pid, and an unreadable file do not. The live process is this test
// binary re-executed with argv "ttorch -test.run=… watch", so the real ps probes run.
func TestStandbyHolder_SeesALiveHandArmedWatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.pid")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestStandbyHelperProcess$", "watch")
	cmd.Args[0] = "ttorch"
	cmd.Env = append(os.Environ(), "TTORCH_STANDBY_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for !isWatchProcess(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("helper pid %d never looked like a ttorch watch to ps", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	write := func(pid int, token string) {
		if err := os.WriteFile(path, []byte(formatWatchRecord(pid, token)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(pid, "pane:4242:Mon-Jun-29-00:16:32-2026")
	if got, ok := standbyHolder(path, processAlive, isWatchProcess); !ok || got != pid {
		t.Fatalf("live hand-armed watch: standbyHolder = %d,%v; want %d,true", got, ok, pid)
	}
	write(pid, daemonToken)
	if _, ok := standbyHolder(path, processAlive, isWatchProcess); ok {
		t.Fatal("the loop's own record read as standby")
	}
	write(os.Getpid(), "pane:1:x") // alive, but this test binary's ps line is not `ttorch watch`
	if _, ok := standbyHolder(path, processAlive, isWatchProcess); ok {
		t.Fatal("a live pid that is not a ttorch watch read as standby")
	}
	_ = os.Remove(path)
	if _, ok := standbyHolder(path, processAlive, isWatchProcess); ok {
		t.Fatal("a missing pid file read as standby")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	write(pid, "pane:4242:Mon-Jun-29-00:16:32-2026")
	if _, ok := standbyHolder(path, processAlive, isWatchProcess); ok {
		t.Fatal("a dead holder read as standby")
	}
}

// TestStandbyHelperProcess is the stand-in hand-armed watcher for the test above; it does
// nothing unless re-executed with TTORCH_STANDBY_HELPER set, and then just waits to be killed.
func TestStandbyHelperProcess(t *testing.T) {
	if os.Getenv("TTORCH_STANDBY_HELPER") == "" {
		t.Skip("helper process")
	}
	time.Sleep(time.Minute)
}
