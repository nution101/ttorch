package peer

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wireString is one SSH wire-format string: a big-endian length and the bytes.
func wireString(b []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	return append(out, b...)
}

// testControlKey returns a fresh ed25519 public key as an authorized_keys-style line and its
// base64 blob.
func testControlKey(t *testing.T) (line, blob string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blob = base64.StdEncoding.EncodeToString(append(wireString([]byte("ssh-ed25519")), wireString(pub)...))
	return "ssh-ed25519 " + blob + " ttorch-peer-control:" + testParent, blob
}

// TestAuthorizedKeyLine pins the line: the forced command is the absolute path of the binary
// and `peer serve`, quoted, with restrict, then the key, then a comment naming the parent.
func TestAuthorizedKeyLine(t *testing.T) {
	_, blob := testControlKey(t)
	got := AuthorizedKeyLine("/home/ttorch/.ttorch/bin/ttorch", blob, testParent)
	want := `command="/home/ttorch/.ttorch/bin/ttorch peer serve --parent ` + testParent + `",restrict ssh-ed25519 ` + blob + ` ttorch-peer-control:` + testParent
	if got != want {
		t.Errorf("AuthorizedKeyLine =\n%s\nwant\n%s", got, want)
	}
}

// TestParseControlKey takes one ed25519 public key and nothing else: no other type, no options,
// no second line, and a blob that really is an ed25519 key.
func TestParseControlKey(t *testing.T) {
	line, blob := testControlKey(t)
	for _, ok := range []string{line, "ssh-ed25519 " + blob, "ssh-ed25519 " + blob + "\n", "  ssh-ed25519 " + blob + " any comment "} {
		if got, err := ParseControlKey(ok); err != nil || got != blob {
			t.Errorf("ParseControlKey(%q) = %q, %v; want the blob", ok, got, err)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(blob)
	rsa := base64.StdEncoding.EncodeToString(append(wireString([]byte("ssh-rsa")), wireString(make([]byte, 32))...))
	short := base64.StdEncoding.EncodeToString(append(wireString([]byte("ssh-ed25519")), wireString(make([]byte, 31))...))
	trailing := base64.StdEncoding.EncodeToString(append(raw, 0))
	for _, bad := range []string{
		"", "ssh-ed25519", "ssh-rsa " + blob, "ssh-ed25519 " + rsa, "ssh-ed25519 " + short, "ssh-ed25519 " + trailing,
		"ssh-ed25519 !!!!", `command="sh" ssh-ed25519 ` + blob, "ssh-ed25519 " + blob + "\ncommand=\"sh\" ssh-ed25519 " + blob,
		"ssh-ed25519 " + blob + " comment\x1b[2J", "ssh-ed25519\t" + blob + "\rx",
	} {
		if _, err := ParseControlKey(bad); err == nil {
			t.Errorf("ParseControlKey(%q) accepted", bad)
		}
	}
}

// TestServeBinary: the forced command runs the binary named in authorized_keys, so it must be an
// absolute path a shell reads as one word, to an executable regular file that only its owner (this
// account or root) can replace, in a directory no one else can write.
func TestServeBinary(t *testing.T) {
	uid := os.Getuid()
	mk := func(t *testing.T, name string, mode os.FileMode) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := mk(t, "ttorch", 0o755)
	got, err := resolveServeBinary(good, uid)
	if err != nil {
		t.Fatalf("resolveServeBinary(%s) = %v", good, err)
	}
	if want, _ := filepath.EvalSymlinks(good); got != want {
		t.Errorf("resolveServeBinary = %q, want the resolved path %q", got, want)
	}
	link := filepath.Join(t.TempDir(), "ttorch-link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveServeBinary(link, uid); err != nil || filepath.Base(got) != "ttorch" {
		t.Errorf("resolveServeBinary(symlink) = %q, %v; want the file it points at", got, err)
	}

	refused := func(label, path string, uid int, want string) {
		t.Helper()
		_, err := resolveServeBinary(path, uid)
		if err == nil {
			t.Errorf("%s: accepted", label)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want it to say %q", label, err, want)
		}
	}
	refused("relative", "ttorch", uid, "absolute")
	refused("group-writable", mk(t, "ttorch", 0o775), uid, "writable")
	refused("world-writable", mk(t, "ttorch", 0o757), uid, "writable")
	refused("not executable", mk(t, "ttorch", 0o644), uid, "executable")
	refused("another account's", good, uid+1, "owned by")
	refused("a space in the path", mk(t, "tt orch", 0o755), uid, "shell")
	refused("a quote in the path", mk(t, `tt"orch`, 0o755), uid, "shell")
	refused("a dollar in the path", mk(t, "tt$orch", 0o755), uid, "shell")
	refused("missing", filepath.Join(t.TempDir(), "nothing"), uid, "")
	loose := mk(t, "ttorch", 0o755)
	if err := os.Chmod(filepath.Dir(loose), 0o777); err != nil {
		t.Fatal(err)
	}
	refused("in a world-writable directory", loose, uid, "writable")
	dir := t.TempDir()
	refused("a directory", dir, uid, "regular file")
}

// TestInstallControlKey appends the line once, creating ~/.ssh and authorized_keys private if
// they are missing, never splicing onto a last line with no newline, and refuses to touch a file
// or directory someone else could have written, or a file that lists this key another way.
func TestInstallControlKey(t *testing.T) {
	uid := os.Getuid()
	_, blob := testControlKey(t)
	line := AuthorizedKeyLine("/home/ttorch/.ttorch/bin/ttorch", blob, testParent)

	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	res, err := installControlKey(sshDir, line, blob, uid, false)
	if err != nil || res.Added != true || res.StaleControlKeys != 0 {
		t.Fatalf("first install = %+v, %v", res, err)
	}
	keys := filepath.Join(sshDir, "authorized_keys")
	if fi, err := os.Stat(sshDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf(".ssh = %v, %v; want created 0700", fi.Mode(), err)
	}
	if fi, err := os.Stat(keys); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("authorized_keys = %v, %v; want created 0600", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(keys); string(b) != line+"\n" {
		t.Errorf("authorized_keys = %q", b)
	}
	res, err = installControlKey(sshDir, line, blob, uid, false)
	if err != nil || res.Added {
		t.Errorf("second install = %+v, %v; want present, nothing added", res, err)
	}
	if b, _ := os.ReadFile(keys); string(b) != line+"\n" {
		t.Errorf("a second install changed authorized_keys: %q", b)
	}

	// An existing file whose last line has no newline keeps that line whole.
	home = t.TempDir()
	sshDir = filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keys = filepath.Join(sshDir, "authorized_keys")
	_, otherBlob := testControlKey(t)
	existing := "ssh-ed25519 AAAAexisting lead@laptop\ncommand=\"/x/ttorch peer serve\",restrict ssh-ed25519 " + otherBlob + " ttorch-peer-control:" + strings.Repeat("f", 32)
	if err := os.WriteFile(keys, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = installControlKey(sshDir, line, blob, uid, false)
	if err != nil || !res.Added || res.StaleControlKeys != 1 {
		t.Fatalf("install after a line with no newline = %+v, %v; want added, one other parent's key", res, err)
	}
	if b, _ := os.ReadFile(keys); string(b) != existing+"\n"+line+"\n" {
		t.Errorf("authorized_keys = %q", b)
	}

	// The same key already listed another way, say without the forced command, is refused.
	home = t.TempDir()
	sshDir = filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keys = filepath.Join(sshDir, "authorized_keys")
	bare := "ssh-ed25519 " + blob + " no-forced-command\n"
	if err := os.WriteFile(keys, []byte(bare), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installControlKey(sshDir, line, blob, uid, false); err == nil || !strings.Contains(err.Error(), "already lists") {
		t.Errorf("the key listed without the forced command: %v, want refused", err)
	}
	if b, _ := os.ReadFile(keys); string(b) != bare {
		t.Errorf("a refused install changed authorized_keys: %q", b)
	}

	refused := func(label, sshDir string, uid int, want string) {
		t.Helper()
		if _, err := installControlKey(sshDir, line, blob, uid, false); err == nil {
			t.Errorf("%s: accepted", label)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want it to say %q", label, err, want)
		}
	}
	mkSSH := func(t *testing.T, dirMode, fileMode os.FileMode) (string, string) {
		t.Helper()
		sshDir := filepath.Join(t.TempDir(), ".ssh")
		if err := os.Mkdir(sshDir, 0o700); err != nil {
			t.Fatal(err)
		}
		keys := filepath.Join(sshDir, "authorized_keys")
		if err := os.WriteFile(keys, []byte("ssh-ed25519 AAAAexisting lead@laptop\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(keys, fileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sshDir, dirMode); err != nil {
			t.Fatal(err)
		}
		return sshDir, keys
	}
	d, _ := mkSSH(t, 0o770, 0o600)
	refused(".ssh group-writable", d, uid, "writable")
	d, _ = mkSSH(t, 0o700, 0o620)
	refused("authorized_keys group-writable", d, uid, "writable")
	d, _ = mkSSH(t, 0o700, 0o606)
	refused("authorized_keys world-writable", d, uid, "writable")
	d, _ = mkSSH(t, 0o700, 0o600)
	refused("another account's .ssh", d, uid+1, "owned by")
	d, k := mkSSH(t, 0o700, 0o600)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(k); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, k); err != nil {
		t.Fatal(err)
	}
	refused("authorized_keys a symlink", d, uid, "symbolic link")
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Errorf("an install wrote through a symlinked authorized_keys: %q", b)
	}
	d, _ = mkSSH(t, 0o700, 0o600)
	linkDir := filepath.Join(t.TempDir(), ".ssh")
	if err := os.Symlink(d, linkDir); err != nil {
		t.Fatal(err)
	}
	refused(".ssh a symlink", linkDir, uid, "symbolic link")
	refused("a relative .ssh", ".ssh", uid, "absolute")
	if _, err := installControlKey(filepath.Join(t.TempDir(), ".ssh"), line+"\nssh-ed25519 "+blob, blob, uid, false); err == nil {
		t.Error("a line holding a newline was installed")
	}
}

// TestProvisionControlKey installs the line for the running binary: the test binary stands in for
// ttorch, so the forced command names its resolved absolute path.
func TestProvisionControlKey(t *testing.T) {
	pub, blob := testControlKey(t)
	sshDir := filepath.Join(t.TempDir(), ".ssh")
	bin, res, err := ProvisionControlKey(sshDir, pub, testParent, os.Getuid(), false)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if want, _ := filepath.EvalSymlinks(exe); bin != want || !res.Added {
		t.Errorf("ProvisionControlKey = %q, %+v; want %q, added", bin, res, want)
	}
	b, err := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if want := AuthorizedKeyLine(bin, blob, testParent) + "\n"; string(b) != want {
		t.Errorf("authorized_keys = %q, want %q", b, want)
	}
	if _, _, err := ProvisionControlKey(filepath.Join(t.TempDir(), ".ssh"), "ssh-rsa AAAA", testParent, os.Getuid(), false); err == nil {
		t.Error("an rsa key was provisioned")
	}
}

// TestOpenPrivateDir: a directory is opened without following a symlink and checked through the
// descriptor; with create, a missing one is made 0700.
func TestOpenPrivateDir(t *testing.T) {
	uid := os.Getuid()
	base := t.TempDir()
	made := filepath.Join(base, "made")
	d, err := OpenPrivateDir(made, uid, true)
	if err != nil || d == nil {
		t.Fatalf("OpenPrivateDir(create) = %v, %v", d, err)
	}
	d.Close()
	if fi, err := os.Lstat(made); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("created %v (%v), want a 0700 directory", fi.Mode(), err)
	}
	if d, err := OpenPrivateDir(filepath.Join(base, "absent"), uid, false); d != nil || err != nil {
		t.Errorf("a missing directory without create = %v, %v; want nil, nil", d, err)
	}
	if _, err := os.Lstat(filepath.Join(base, "absent")); !os.IsNotExist(err) {
		t.Error("a missing directory was created without create")
	}

	refused := func(label, dir string, uid int, create bool, want string) {
		t.Helper()
		d, err := OpenPrivateDir(dir, uid, create)
		if err == nil {
			d.Close()
			t.Errorf("%s: opened", label)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want it to say %q", label, err, want)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(made, link); err != nil {
		t.Fatal(err)
	}
	refused("a symlink to a private directory", link, uid, false, "symbolic link")
	refused("a symlink, with create", link, uid, true, "symbolic link")
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	refused("a file", file, uid, true, "not a directory")
	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o770); err != nil {
		t.Fatal(err)
	}
	refused("a group-writable directory", loose, uid, false, "writable")
	refused("another account's directory", made, uid+1, false, "owned by")
	refused("a relative path", "made", uid, false, "absolute")
}

// TestOpenAtStaysInTheDirectoryItChecked: once a directory is open, a file opened through it is in
// that directory, even if its path is renamed away and a symlink to somewhere else put in its
// place, and a symlink inside it is never followed.
func TestOpenAtStaysInTheDirectoryItChecked(t *testing.T) {
	uid := os.Getuid()
	base := t.TempDir()
	dir := filepath.Join(base, ".ssh")
	d, err := OpenPrivateDir(dir, uid, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	elsewhere := t.TempDir()
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	f, err := OpenAt(d, "authorized_keys", os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("x\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(filepath.Join(dir+".moved", "authorized_keys")); err != nil {
		t.Errorf("the file is not in the directory that was checked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "authorized_keys")); !os.IsNotExist(err) {
		t.Errorf("the write followed the swapped-in symlink to %s", elsewhere)
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir+".moved", "link")); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenAt(d, "link", os.O_WRONLY|os.O_APPEND, 0); err == nil {
		f.Close()
		t.Error("OpenAt followed a symlink")
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../x"} {
		if f, err := OpenAt(d, bad, os.O_RDONLY, 0); err == nil {
			f.Close()
			t.Errorf("OpenAt(%q) opened something", bad)
		}
	}
}

// TestInstallControlKeyReplacing: with replace (adopt --force), every other control key's line
// goes, an older line for this same key included, and every other line stays as it was. The file
// is rewritten through a new file renamed over it in the directory that was checked, reports the
// parent each removed line named, and leaves no temporary file behind. A line that lists this key
// without a control key's comment is still refused, and a file that changes between the read and
// the rename is refused with the change kept.
func TestInstallControlKeyReplacing(t *testing.T) {
	uid := os.Getuid()
	_, blob := testControlKey(t)
	line := AuthorizedKeyLine("/home/ttorch/.ttorch/bin/ttorch", blob, testParent)
	_, otherBlob := testControlKey(t)
	other := strings.Repeat("e", 32)
	own := "ssh-ed25519 AAAAexisting lead@laptop"
	oldOther := AuthorizedKeyLine("/x/ttorch", otherBlob, other)
	oldSame := `command="/x/ttorch peer serve",restrict ssh-ed25519 ` + blob + " " + ControlKeyComment(testParent)
	setup := func(t *testing.T, content string) (string, string) {
		t.Helper()
		sshDir := filepath.Join(t.TempDir(), ".ssh")
		if err := os.Mkdir(sshDir, 0o700); err != nil {
			t.Fatal(err)
		}
		keys := filepath.Join(sshDir, "authorized_keys")
		if err := os.WriteFile(keys, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return sshDir, keys
	}
	onlyKeys := func(t *testing.T, sshDir string) {
		t.Helper()
		ents, _ := os.ReadDir(sshDir)
		if len(ents) != 1 || ents[0].Name() != "authorized_keys" {
			var names []string
			for _, e := range ents {
				names = append(names, e.Name())
			}
			t.Errorf(".ssh holds %v; want only authorized_keys", names)
		}
	}

	sshDir, keys := setup(t, own+"\n"+oldOther+"\n# a note\n"+oldSame)
	res, err := installControlKey(sshDir, line, blob, uid, true)
	if err != nil || !res.Added || res.StaleControlKeys != 0 || strings.Join(res.Removed, ",") != other+","+testParent {
		t.Fatalf("replacing install = %+v, %v; want added, removing %s and %s", res, err, other, testParent)
	}
	if b, _ := os.ReadFile(keys); string(b) != own+"\n# a note\n"+line+"\n" {
		t.Errorf("authorized_keys = %q", b)
	}
	if fi, err := os.Lstat(keys); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("authorized_keys after the rewrite = %v, %v; want a 0600 regular file", fi.Mode(), err)
	}
	onlyKeys(t, sshDir)

	// Nothing to remove and the line present: nothing is written at all.
	before, _ := os.Lstat(keys)
	if res, err := installControlKey(sshDir, line, blob, uid, true); err != nil || res.Added || len(res.Removed) != 0 {
		t.Errorf("a repeat = %+v, %v; want present, nothing removed", res, err)
	}
	if after, _ := os.Lstat(keys); !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Error("a repeat with nothing to remove rewrote authorized_keys")
	}

	// The line already there and another parent's beside it: that one goes, the line stays once.
	sshDir, keys = setup(t, line+"\n"+oldOther+"\n")
	if res, err := installControlKey(sshDir, line, blob, uid, true); err != nil || res.Added || strings.Join(res.Removed, ",") != other {
		t.Errorf("replacing with the line present = %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(keys); string(b) != line+"\n" {
		t.Errorf("authorized_keys = %q", b)
	}

	// This key listed as a plain key, with no forced command, is someone's own entry: refused.
	plain := "ssh-ed25519 " + blob + " lead@laptop"
	sshDir, keys = setup(t, plain+"\n"+oldOther+"\n")
	if _, err := installControlKey(sshDir, line, blob, uid, true); err == nil || !strings.Contains(err.Error(), "another way") {
		t.Errorf("replacing over a plain listing of the key: %v", err)
	}
	if b, _ := os.ReadFile(keys); string(b) != plain+"\n"+oldOther+"\n" {
		t.Errorf("a refused replace changed authorized_keys: %q", b)
	}

	// authorized_keys a symlink: refused, the target untouched.
	sshDir, keys = setup(t, "")
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte(oldOther+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keys); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, keys); err != nil {
		t.Fatal(err)
	}
	if _, err := installControlKey(sshDir, line, blob, uid, true); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("replacing through a symlinked authorized_keys: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != oldOther+"\n" {
		t.Errorf("the symlink's target changed: %q", b)
	}
	if fi, _ := os.Lstat(keys); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}

	// Someone appends between the read and the rename: refused, and their line survives.
	sshDir, keys = setup(t, own+"\n"+oldOther+"\n")
	late := "ssh-ed25519 AAAAlate someone@else"
	afterAuthorizedKeysRead = func() {
		f, err := os.OpenFile(keys, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Error(err)
			return
		}
		f.WriteString(late + "\n")
		f.Close()
	}
	t.Cleanup(func() { afterAuthorizedKeysRead = nil })
	if _, err := installControlKey(sshDir, line, blob, uid, true); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("a file that changed during the rewrite: %v", err)
	}
	afterAuthorizedKeysRead = nil
	if b, _ := os.ReadFile(keys); string(b) != own+"\n"+oldOther+"\n"+late+"\n" {
		t.Errorf("the change made during the rewrite was lost: %q", b)
	}
	onlyKeys(t, sshDir)
}

// TestInstallControlKeyIsSerialized: two installs at once take turns. The first is held after it
// has read the file; the second must not read it until the first has written, or both would see
// the key absent and append it twice.
func TestInstallControlKeyIsSerialized(t *testing.T) {
	uid := os.Getuid()
	_, blob := testControlKey(t)
	line := AuthorizedKeyLine("/home/ttorch/.ttorch/bin/ttorch", blob, testParent)
	sshDir := filepath.Join(t.TempDir(), ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	firstRead, release := make(chan struct{}), make(chan struct{})
	afterAuthorizedKeysRead = func() {
		if reads.Add(1) == 1 {
			close(firstRead)
			<-release
		}
	}
	t.Cleanup(func() { afterAuthorizedKeysRead = nil })

	type outcome struct {
		res InstallResult
		err error
	}
	first, second := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		res, err := installControlKey(sshDir, line, blob, uid, false)
		first <- outcome{res, err}
	}()
	<-firstRead
	go func() {
		res, err := installControlKey(sshDir, line, blob, uid, false)
		second <- outcome{res, err}
	}()
	select {
	case o := <-second:
		t.Errorf("the second install finished while the first held the file: %+v, %v", o.res, o.err)
		close(release)
		<-first
	case <-time.After(300 * time.Millisecond):
		close(release)
		a, b := <-first, <-second
		if a.err != nil || b.err != nil || !a.res.Added || b.res.Added {
			t.Errorf("installs = %+v, %v and %+v, %v; want the first added and the second to find it", a.res, a.err, b.res, b.err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(sshDir, "authorized_keys")); string(b) != line+"\n" {
		t.Errorf("authorized_keys = %q; want the line once", b)
	}
}

// TestLockDirWaitsThenGivesUp: a second holder waits for the first, and gives up with a named
// error once its wait runs out; closing the first's directory lets it in.
func TestLockDirWaitsThenGivesUp(t *testing.T) {
	dir := t.TempDir()
	open := func() *os.File {
		d, err := OpenPrivateDir(dir, os.Getuid(), false)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	first, second := open(), open()
	defer second.Close()
	if err := lockDir(first, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := lockDir(second, 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "still locked") {
		t.Errorf("a second lock while the first is held: %v", err)
	}
	first.Close()
	if err := lockDir(second, time.Second); err != nil {
		t.Errorf("the lock once the first holder closed its directory: %v", err)
	}
}
