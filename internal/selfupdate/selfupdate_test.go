package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// --- Apply: download + checksum-verify + atomic-replace, fully offline ---

// roundTripFunc lets a test stand in for the network. Apply builds real
// https://github.com/... URLs; this transport answers them from canned bytes
// without ever opening a socket, so the HTTPS-only download guard is satisfied.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// tarGzWith builds a .tar.gz whose single entry is named `name` with `content`.
func tarGzWith(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const testAsset = "ttorch-v1.2.3-linux-amd64.tar.gz"

// applyHarness wires a Config whose Client returns canned asset/checksums
// responses, plus a target file (seeded with "OLD") to be replaced.
type applyHarness struct {
	cfg       Config
	target    string
	requested []string
}

func newApplyHarness(t *testing.T, assetStatus int, assetBody []byte, sumsStatus int, sumsBody []byte) *applyHarness {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "ttorch")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &applyHarness{target: target}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h.requested = append(h.requested, r.URL.String())
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			return resp(sumsStatus, sumsBody), nil
		case strings.HasSuffix(r.URL.Path, "/"+testAsset):
			return resp(assetStatus, assetBody), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})
	h.cfg = Config{
		Repo:           "o/r",
		AssetName:      testAsset,
		ExecutablePath: target,
		Client:         &http.Client{Transport: transport},
	}
	return h
}

func (h *applyHarness) targetContent(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(h.target)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApply_Success(t *testing.T) {
	bin := tarGzWith(t, "ttorch", []byte("NEW-BINARY"))
	sums := sha256hex(bin) + "  " + testAsset + "\n"
	h := newApplyHarness(t, http.StatusOK, bin, http.StatusOK, []byte(sums))

	if err := h.cfg.Apply("v1.2.3"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.targetContent(t); got != "NEW-BINARY" {
		t.Fatalf("target content = %q, want NEW-BINARY", got)
	}
	wantAsset := "https://github.com/o/r/releases/download/v1.2.3/" + testAsset
	wantSums := "https://github.com/o/r/releases/download/v1.2.3/checksums.txt"
	if !contains(h.requested, wantAsset) || !contains(h.requested, wantSums) {
		t.Fatalf("requested = %v, want %q and %q", h.requested, wantAsset, wantSums)
	}
}

func TestApply_ChecksumMismatch(t *testing.T) {
	bin := tarGzWith(t, "ttorch", []byte("NEW-BINARY"))
	sums := strings.Repeat("0", 64) + "  " + testAsset + "\n" // wrong digest
	h := newApplyHarness(t, http.StatusOK, bin, http.StatusOK, []byte(sums))

	err := h.cfg.Apply("v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Apply err = %v, want checksum mismatch", err)
	}
	if got := h.targetContent(t); got != "OLD" {
		t.Fatalf("target replaced despite checksum mismatch: %q", got)
	}
}

func TestApply_NoChecksumEntry(t *testing.T) {
	bin := tarGzWith(t, "ttorch", []byte("NEW-BINARY"))
	sums := sha256hex(bin) + "  some-other-asset.tar.gz\n"
	h := newApplyHarness(t, http.StatusOK, bin, http.StatusOK, []byte(sums))

	err := h.cfg.Apply("v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("Apply err = %v, want no checksum", err)
	}
	if got := h.targetContent(t); got != "OLD" {
		t.Fatalf("target replaced: %q", got)
	}
}

func TestApply_AssetDownloadFails(t *testing.T) {
	h := newApplyHarness(t, http.StatusNotFound, nil, http.StatusOK, []byte("x"))
	err := h.cfg.Apply("v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "download asset") {
		t.Fatalf("Apply err = %v, want download asset", err)
	}
	if got := h.targetContent(t); got != "OLD" {
		t.Fatalf("target replaced: %q", got)
	}
}

func TestApply_ChecksumsDownloadFails(t *testing.T) {
	bin := tarGzWith(t, "ttorch", []byte("NEW-BINARY"))
	h := newApplyHarness(t, http.StatusOK, bin, http.StatusNotFound, nil)
	err := h.cfg.Apply("v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "download checksums") {
		t.Fatalf("Apply err = %v, want download checksums", err)
	}
	if got := h.targetContent(t); got != "OLD" {
		t.Fatalf("target replaced: %q", got)
	}
}

func TestApply_CorruptArchive(t *testing.T) {
	bad := []byte("this is not a gzip archive")
	sums := sha256hex(bad) + "  " + testAsset + "\n" // checksum matches the garbage
	h := newApplyHarness(t, http.StatusOK, bad, http.StatusOK, []byte(sums))

	// Verification passes, extraction must fail, and nothing is replaced.
	if err := h.cfg.Apply("v1.2.3"); err == nil {
		t.Fatal("Apply succeeded on a corrupt archive")
	}
	if got := h.targetContent(t); got != "OLD" {
		t.Fatalf("target replaced from corrupt archive: %q", got)
	}
}

func TestApply_BinaryNotInArchive(t *testing.T) {
	bin := tarGzWith(t, "README.md", []byte("not the binary"))
	sums := sha256hex(bin) + "  " + testAsset + "\n"
	h := newApplyHarness(t, http.StatusOK, bin, http.StatusOK, []byte(sums))

	err := h.cfg.Apply("v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "not found in archive") {
		t.Fatalf("Apply err = %v, want not found in archive", err)
	}
	if got := h.targetContent(t); got != "OLD" {
		t.Fatalf("target replaced: %q", got)
	}
}

func TestDownload_RefusesNonHTTPS(t *testing.T) {
	if _, err := download(&http.Client{}, "http://example.com/x"); err == nil ||
		!strings.Contains(err.Error(), "non-HTTPS") {
		t.Fatalf("download(http://) err = %v, want non-HTTPS refusal", err)
	}
}

func TestLatest_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := githubWeb
	githubWeb = srv.URL
	defer func() { githubWeb = old }()

	if _, err := Latest("o/r", nil); err == nil || !strings.Contains(err.Error(), "github returned") {
		t.Fatalf("Latest err = %v, want a github returned <status> error", err)
	}
}

func TestLatest_RedirectStatusCodes(t *testing.T) {
	codes := []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	}
	for _, code := range codes {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://github.com/o/r/releases/tag/v3.2.1")
			w.WriteHeader(code)
		}))
		old := githubWeb
		githubWeb = srv.URL
		tag, err := Latest("o/r", nil)
		srv.Close()
		githubWeb = old
		if err != nil || tag != "v3.2.1" {
			t.Fatalf("Latest with redirect %d = (%q,%v), want (v3.2.1,nil)", code, tag, err)
		}
	}
}

func TestReplaceExecutable_PlainFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ttorch")
	if err := os.WriteFile(target, []byte("OLD"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceExecutable(target, []byte("NEW")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "NEW" {
		t.Fatalf("content = %q, want NEW", b)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("perm = %v, want 0700 preserved", fi.Mode().Perm())
	}
	// The atomic rename must leave no temp file behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ttorch-update-") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestReplaceExecutable_CreatesMissingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ttorch") // does not exist yet
	if err := ReplaceExecutable(target, []byte("NEW")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "NEW" {
		t.Fatalf("content = %q, want NEW", b)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("perm = %v, want default 0755", fi.Mode().Perm())
	}
}
