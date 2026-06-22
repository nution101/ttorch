package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"1.0.0", "v1.0.0", 0},
		{"v1.2.0", "v1.1.9", 1},
		{"v1.1.9", "v1.2.0", -1},
		{"v2.0.0", "v1.9.9", 1},
		{"v0.1.0", "v0.1.1", -1},
		{"v1.0.0-beta", "v1.0.0", 0}, // pre-release ignored for ordering
		{"v1.0.1", "v1.0.0+build", 1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestReplaceExecutable_PreservesPermAndFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "bin", "orcha")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH symlink pointing at the real binary.
	link := filepath.Join(dir, "orcha")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if err := ReplaceExecutable(link, []byte("new-binary")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new-binary" {
		t.Fatalf("real binary content = %q, want new-binary (symlink not followed?)", b)
	}
	fi, err := os.Stat(real)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("perm = %v, want 0755", fi.Mode().Perm())
	}
	// The symlink itself must still be a symlink (we rewrote the target, not it).
	lfi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if lfi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced with a regular file")
	}
}

func TestExtractBinary_UnsupportedFormat(t *testing.T) {
	if _, err := extractBinary("orcha.zip", []byte("x")); err == nil {
		t.Fatal("expected error for unsupported format")
	}
}

func TestChecksumFor(t *testing.T) {
	sums := "abc123  orcha-v1-darwin-arm64.tar.gz\ndef456 *orcha-v1-linux-amd64.tar.gz\n"
	if got, ok := checksumFor(sums, "orcha-v1-darwin-arm64.tar.gz"); !ok || got != "abc123" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if got, ok := checksumFor(sums, "orcha-v1-linux-amd64.tar.gz"); !ok || got != "def456" {
		t.Fatalf("star-prefixed name: got %q ok=%v", got, ok)
	}
	if _, ok := checksumFor(sums, "missing"); ok {
		t.Fatal("expected miss")
	}
}
