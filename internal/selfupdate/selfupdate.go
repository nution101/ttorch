// Package selfupdate implements `ttorch update`: check the latest GitHub release,
// download + checksum-verify the asset, and atomically replace the running
// binary — following the PATH symlink so the real file is rewritten, and clearing
// the macOS quarantine xattr so Gatekeeper does not block the updated binary.
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrNoReleases indicates the repo has no published release yet.
var ErrNoReleases = errors.New("no releases published yet")

// githubWeb is the github.com base (overridable in tests). The release "latest"
// lookup uses the web redirect here, NOT api.github.com, to avoid the
// unauthenticated API rate limit (60 requests/hour/IP).
var githubWeb = "https://github.com"

const maxDownload = 256 << 20 // 256 MiB cap

// Config parameterizes the updater for ttorch's repo + asset naming.
type Config struct {
	Repo           string // "owner/ttorch"
	CurrentVersion string
	ExecutablePath string
	AssetName      string // e.g. ttorch-v0.1.0-darwin-arm64.tar.gz
	Client         *http.Client
}

func (c Config) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Latest returns the newest release tag, or ErrNoReleases. It reads the tag from
// the github.com /releases/latest redirect — the Location header points at
// /releases/tag/<tag> — instead of api.github.com, which is rate-limited to 60
// unauthenticated requests/hour/IP and would 403 after a few `ttorch update`s.
func Latest(repo string, client *http.Client) (string, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	// Capture the redirect instead of following it.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := c.Head(githubWeb + "/" + repo + "/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		if tag, ok := tagFromLocation(resp.Header.Get("Location")); ok {
			return tag, nil
		}
		// A repo with no releases redirects to /releases (no /tag/ segment).
		return "", ErrNoReleases
	case http.StatusNotFound:
		return "", ErrNoReleases
	default:
		return "", fmt.Errorf("github returned %s", resp.Status)
	}
}

// tagFromLocation extracts the release tag from a /releases/latest redirect
// Location (e.g. https://github.com/owner/repo/releases/tag/v1.2.3 -> v1.2.3).
func tagFromLocation(loc string) (string, bool) {
	const marker = "/releases/tag/"
	i := strings.LastIndex(loc, marker)
	if i < 0 {
		return "", false
	}
	if tag := strings.Trim(loc[i+len(marker):], "/"); tag != "" {
		return tag, true
	}
	return "", false
}

// Apply downloads the release asset for tag, verifies its sha256 against the
// release's checksums.txt, extracts the ttorch binary, and atomically replaces
// the running executable.
func (c Config) Apply(tag string) error {
	base := "https://github.com/" + c.Repo + "/releases/download/" + tag + "/"
	data, err := download(c.client(), base+c.AssetName)
	if err != nil {
		return fmt.Errorf("download asset: %w", err)
	}
	sums, err := download(c.client(), base+"checksums.txt")
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	want, ok := checksumFor(string(sums), c.AssetName)
	if !ok {
		return fmt.Errorf("no checksum for %s in checksums.txt", c.AssetName)
	}
	got := sha256hex(data)
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch: got %s want %s", got, want)
	}
	bin, err := extractBinary(c.AssetName, data)
	if err != nil {
		return err
	}
	return ReplaceExecutable(c.ExecutablePath, bin)
}

func download(client *http.Client, url string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("refusing non-HTTPS url: %s", url)
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload))
}

func checksumFor(sums, asset string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], true
		}
	}
	return "", false
}

func extractBinary(asset string, data []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(asset, ".tar.gz"), strings.HasSuffix(asset, ".tgz"):
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			name := hdr.Name
			if i := strings.LastIndexByte(name, '/'); i >= 0 {
				name = name[i+1:]
			}
			if name == "ttorch" || name == "ttorch.exe" {
				return io.ReadAll(io.LimitReader(tr, maxDownload))
			}
		}
		return nil, errors.New("ttorch binary not found in archive")
	default:
		return nil, fmt.Errorf("unsupported asset format: %s", asset)
	}
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CompareVersions returns -1, 0, or 1 for a<b, a==b, a>b using semver-lite
// (leading "v" ignored; pre-release/build metadata ignored for ordering).
func CompareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(v string) [3]int {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, _ := strconv.Atoi(strings.TrimSpace(part))
		out[i] = n
	}
	return out
}
