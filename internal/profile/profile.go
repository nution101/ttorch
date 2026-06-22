// Package profile derives a repository's "project profile" — its stack, exact
// build/test/lint commands, conventions, layout, and a few representative exemplar
// files — and writes it as a managed block in AGENTS.md so workers match the repo's
// style. It is deterministic and dependency-free: a filesystem scan, no embeddings
// and no RAG.
package profile

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	markerBegin = "<!-- BEGIN orcha-profile -->"
	markerEnd   = "<!-- END orcha-profile -->"
)

// Profile is the derived, render-ready summary of a repository.
type Profile struct {
	Stack     string   // "Go", "Node.js", "Python", or "" if unknown
	Build     []string // exact build commands
	Test      []string // exact test commands
	Lint      []string // exact lint/format commands
	TestHint  string   // test framework / where tests live
	Layout    []string // notable top-level directories
	Exemplars []string // representative source files (repo-relative)
	Notes     []string // non-obvious detected facts (config files to honor, etc.)
}

var noiseDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".orcha": true, ".idea": true,
	".vscode": true, ".next": true, "out": true,
}

// Detect scans repoDir and returns its profile.
func Detect(repoDir string) Profile {
	var p Profile
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(repoDir, name))
		return err == nil
	}

	switch {
	case has("go.mod"):
		p.Stack = "Go"
		p.Build = []string{"go build ./..."}
		p.Test = []string{"go test ./..."}
		p.Lint = []string{"gofmt -l .", "go vet ./..."}
		p.TestHint = "standard `testing` package; tests in *_test.go beside sources"
		p.Exemplars = pickExemplars(repoDir, []string{".go"})
	case has("package.json"):
		p.Stack = "Node.js"
		scripts := packageScripts(filepath.Join(repoDir, "package.json"))
		if _, ok := scripts["build"]; ok {
			p.Build = []string{"npm run build"}
		}
		if _, ok := scripts["test"]; ok {
			p.Test = []string{"npm test"}
			p.TestHint = "via the `test` script in package.json"
		}
		if _, ok := scripts["lint"]; ok {
			p.Lint = []string{"npm run lint"}
		}
		p.Exemplars = pickExemplars(repoDir, []string{".ts", ".tsx", ".js", ".jsx"})
	case has("pyproject.toml") || has("setup.py") || has("requirements.txt"):
		p.Stack = "Python"
		if has("pyproject.toml") && fileContains(filepath.Join(repoDir, "pyproject.toml"), "tool.ruff") {
			p.Lint = []string{"ruff check ."}
		}
		p.Test = []string{"pytest"}
		p.TestHint = "pytest; tests under tests/ or test_*.py"
		p.Exemplars = pickExemplars(repoDir, []string{".py"})
	default:
		// Unknown stack: still capture layout + exemplars from the dominant code ext.
		p.Exemplars = pickExemplars(repoDir, dominantExts(repoDir))
	}

	p.Layout = topDirs(repoDir)
	p.Notes = detectConfigs(repoDir)
	return p
}

// Render produces the AGENTS.md block body for a profile (only populated lines).
func Render(p Profile) string {
	var b strings.Builder
	b.WriteString("Maintained by orcha (`orcha profile`). The repo's stack, commands, and\n")
	b.WriteString("conventions — match these when changing code here.\n\n")
	line := func(label string, v string) {
		if v != "" {
			b.WriteString("- " + label + ": " + v + "\n")
		}
	}
	cmds := func(label string, xs []string) {
		if len(xs) == 0 {
			return
		}
		quoted := make([]string, len(xs))
		for i, x := range xs {
			quoted[i] = "`" + x + "`"
		}
		line(label, strings.Join(quoted, ", "))
	}
	line("Stack", p.Stack)
	cmds("Build", p.Build)
	cmds("Test", p.Test)
	cmds("Lint/format", p.Lint)
	line("Tests", p.TestHint)
	if len(p.Layout) > 0 {
		line("Layout", strings.Join(p.Layout, ", "))
	}
	if len(p.Exemplars) > 0 {
		line("Match the style of", strings.Join(p.Exemplars, ", "))
	}
	for _, n := range p.Notes {
		line("Note", n)
	}
	return b.String()
}

// Apply detects the profile for repoDir and upserts it into AGENTS.md.
func Apply(repoDir string) (Profile, error) {
	p := Detect(repoDir)
	return p, writeBlock(filepath.Join(repoDir, "AGENTS.md"), Render(p))
}

func writeBlock(agentsPath, body string) error {
	block := markerBegin + "\n" + strings.TrimRight(body, "\n") + "\n" + markerEnd
	existing, err := os.ReadFile(agentsPath)
	if os.IsNotExist(err) {
		return atomicWrite(agentsPath, []byte("# Project guidance\n\n"+block+"\n"))
	}
	if err != nil {
		return err
	}
	text := string(existing)
	bi := strings.Index(text, markerBegin)
	ei := strings.Index(text, markerEnd)
	if bi >= 0 && ei > bi {
		return atomicWrite(agentsPath, []byte(text[:bi]+block+text[ei+len(markerEnd):]))
	}
	sep := "\n"
	if !strings.HasSuffix(text, "\n") {
		sep = "\n\n"
	}
	return atomicWrite(agentsPath, []byte(text+sep+block+"\n"))
}

// --- detection helpers ---

func packageScripts(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return nil
	}
	return pkg.Scripts
}

func topDirs(repoDir string) []string {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !noiseDirs[e.Name()] && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, e.Name()+"/")
		}
	}
	sort.Strings(dirs)
	if len(dirs) > 8 {
		dirs = dirs[:8]
	}
	return dirs
}

func detectConfigs(repoDir string) []string {
	var notes []string
	checks := []struct{ glob, note string }{
		{".editorconfig", "honor .editorconfig"},
		{".prettierrc", "honor Prettier config"},
		{".eslintrc", "honor ESLint config"},
		{".golangci.yml", "honor golangci-lint config"},
		{".golangci.yaml", "honor golangci-lint config"},
	}
	for _, c := range checks {
		matches, _ := filepath.Glob(filepath.Join(repoDir, c.glob+"*"))
		if len(matches) > 0 {
			notes = append(notes, c.note)
		}
	}
	return notes
}

func fileContains(path, sub string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), sub)
}

// dominantExts returns the most common code extension(s) in a repo.
func dominantExts(repoDir string) []string {
	counts := map[string]int{}
	code := map[string]bool{".go": true, ".ts": true, ".js": true, ".py": true, ".rb": true, ".rs": true, ".java": true}
	_ = walkSource(repoDir, func(rel string, _ fs.DirEntry) {
		ext := filepath.Ext(rel)
		if code[ext] {
			counts[ext]++
		}
	})
	best, bestN := "", 0
	for ext, n := range counts {
		if n > bestN {
			best, bestN = ext, n
		}
	}
	if best == "" {
		return nil
	}
	return []string{best}
}

// pickExemplars chooses up to 3 representative source files of the given extensions
// — sized near the median (avoiding the tiniest and the likely-generated largest),
// excluding tests and generated files.
func pickExemplars(repoDir string, exts []string) []string {
	if len(exts) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, e := range exts {
		want[e] = true
	}
	type cand struct {
		rel  string
		size int64
	}
	var cands []cand
	_ = walkSource(repoDir, func(rel string, d fs.DirEntry) {
		if !want[filepath.Ext(rel)] {
			return
		}
		base := filepath.Base(rel)
		if strings.Contains(base, "_test.") || strings.Contains(base, ".test.") ||
			strings.Contains(base, ".spec.") || strings.Contains(base, ".min.") {
			return
		}
		info, err := d.Info()
		if err != nil {
			return
		}
		cands = append(cands, cand{rel, info.Size()})
	})
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].size != cands[j].size {
			return cands[i].size < cands[j].size
		}
		return cands[i].rel < cands[j].rel
	})
	if len(cands) <= 3 {
		out := make([]string, len(cands))
		for i, c := range cands {
			out[i] = c.rel
		}
		return out
	}
	mid := len(cands) / 2
	idxs := []int{mid - 1, mid, mid + 1}
	var out []string
	for _, i := range idxs {
		out = append(out, cands[i].rel)
	}
	sort.Strings(out)
	return out
}

// walkSource visits non-noise files under repoDir, calling fn with the repo-relative
// path. Read-only; metadata only.
func walkSource(repoDir string, fn func(rel string, d fs.DirEntry)) error {
	return filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() != "." && (noiseDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) && path != repoDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoDir, path)
		if err != nil {
			return nil
		}
		fn(rel, d)
		return nil
	})
}

func atomicWrite(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".orcha-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
