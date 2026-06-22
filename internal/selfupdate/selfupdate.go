// Package selfupdate implements `orcha update`: check the latest GitHub release,
// download + checksum-verify the asset, and atomically replace the running
// binary. The atomic-replace + macOS-quarantine logic is adapted from the
// vendored upstream update engine.
package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

const maxDownload = 256 << 20 // 256 MiB cap

// Config parameterizes the updater for orcha's repo + asset naming.
type Config struct {
	Repo           string // "owner/orcha"
	CurrentVersion string
	ExecutablePath string
	AssetName      string // e.g. orcha-v0.1.0-darwin-arm64.tar.gz
	Client         *http.Client
}

func (c Config) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Latest returns the newest release tag, or ErrNoReleases.
func Latest(repo string, client *http.Client) (string, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNoReleases
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github returned %s", resp.Status)
	}
	var out struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	if out.TagName == "" {
		return "", ErrNoReleases
	}
	return out.TagName, nil
}

// Apply downloads the release asset for tag, verifies its sha256 against the
// release's checksums.txt, extracts the orcha binary, and atomically replaces
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
			if name == "orcha" || name == "orcha.exe" {
				return io.ReadAll(io.LimitReader(tr, maxDownload))
			}
		}
		return nil, errors.New("orcha binary not found in archive")
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
