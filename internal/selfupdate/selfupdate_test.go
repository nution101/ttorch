package selfupdate

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTagFromLocation(t *testing.T) {
	cases := []struct {
		loc     string
		wantTag string
		wantOK  bool
	}{
		{"https://github.com/o/r/releases/tag/v0.1.7", "v0.1.7", true},
		{"/o/r/releases/tag/v1.2.3", "v1.2.3", true},
		{"https://github.com/o/r/releases/tag/v0.1.7/", "v0.1.7", true},
		{"https://github.com/o/r/releases", "", false}, // no releases
		{"https://github.com/o/r/releases/tag/", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		tag, ok := tagFromLocation(c.loc)
		if tag != c.wantTag || ok != c.wantOK {
			t.Errorf("tagFromLocation(%q) = (%q,%v), want (%q,%v)", c.loc, tag, ok, c.wantTag, c.wantOK)
		}
	}
}

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/has/rel/releases/latest":
			http.Redirect(w, r, "https://github.com/has/rel/releases/tag/v9.9.9", http.StatusFound)
		case "/no/rel/releases/latest":
			http.Redirect(w, r, "https://github.com/no/rel/releases", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	old := githubWeb
	githubWeb = srv.URL
	defer func() { githubWeb = old }()

	if tag, err := Latest("has/rel", nil); err != nil || tag != "v9.9.9" {
		t.Fatalf("Latest(has/rel) = (%q,%v), want (v9.9.9,nil)", tag, err)
	}
	if _, err := Latest("no/rel", nil); err != ErrNoReleases {
		t.Fatalf("Latest(no/rel) err = %v, want ErrNoReleases", err)
	}
	if _, err := Latest("missing/repo", nil); err != ErrNoReleases {
		t.Fatalf("Latest(missing) err = %v, want ErrNoReleases (404)", err)
	}
}

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
	real := filepath.Join(dir, "bin", "ttorch")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH symlink pointing at the real binary.
	link := filepath.Join(dir, "ttorch")
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
	if _, err := extractBinary("ttorch.zip", []byte("x")); err == nil {
		t.Fatal("expected error for unsupported format")
	}
}

func TestChecksumFor(t *testing.T) {
	sums := "abc123  ttorch-v1-darwin-arm64.tar.gz\ndef456 *ttorch-v1-linux-amd64.tar.gz\n"
	if got, ok := checksumFor(sums, "ttorch-v1-darwin-arm64.tar.gz"); !ok || got != "abc123" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if got, ok := checksumFor(sums, "ttorch-v1-linux-amd64.tar.gz"); !ok || got != "def456" {
		t.Fatalf("star-prefixed name: got %q ok=%v", got, ok)
	}
	if _, ok := checksumFor(sums, "missing"); ok {
		t.Fatal("expected miss")
	}
}
