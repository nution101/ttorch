package installer

import (
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	ttorch "github.com/nution101/ttorch"
	"github.com/nution101/ttorch/internal/review"
)

// TestClassifierCoversEveryInstalledSubtree ties the trust gate's size classifier to what this
// package actually installs. A file the installer writes into a session's directories is read
// by every future agent session on the machine, so a diff touching its embedded source must
// keep the security reviewer. review.Classify decides that from a hand-maintained list of
// repository-rooted subtrees, and desiredFiles decides what is installed from the switch
// below; the two are separate enumerations and they have already drifted once. The classifier
// was anchored on content/agents and content/skills while desiredFiles also routed
// content/commands and content/assets, so content/assets/AGENTS.global.md, the payload merged
// into the global AGENTS.md block, classified as inert prose and dropped the reviewer.
//
// Rather than restate either list, this walks the real payload, learns which source paths come
// out the far side of desiredFiles, and asserts the classifier does not call any of them
// prose. Adding a subtree to the switch without adding it to agentInstructionRoots fails here.
func TestClassifierCoversEveryInstalledSubtree(t *testing.T) {
	// Mirror the payload with each file's own source path as its content, so the destinations
	// desiredFiles returns can be mapped back to the sources that produced them.
	//
	// THIS MIRROR ASSUMES ROUTING IS PATH-ONLY. Substituting the content is what makes the
	// mapping back possible, so if desiredFiles ever routes on what a file CONTAINS rather
	// than where it sits, frontmatter or a marker line or a shebang, this test stops
	// reflecting reality and will not fail to tell you. Route on content and this mirror has
	// to change with it.
	mirror := fstest.MapFS{}
	if err := fs.WalkDir(ttorch.Content(), embedRoot, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		mirror[fp] = &fstest.MapFile{Data: []byte(fp)}
		return nil
	}); err != nil {
		t.Fatalf("walk payload: %v", err)
	}
	if len(mirror) == 0 {
		t.Fatal("payload mirror is empty, so this test would pass vacuously")
	}

	desired, global, err := desiredFiles(mirror, sandbox(t))
	if err != nil {
		t.Fatalf("desiredFiles: %v", err)
	}

	installed := make([]string, 0, len(desired)+1)
	for _, src := range desired {
		installed = append(installed, string(src))
	}
	if global != nil {
		installed = append(installed, string(global))
	}
	sort.Strings(installed)

	// Key on the TOP-LEVEL subtree, which is what agentInstructionRoots is keyed on. Keying on
	// the source's own directory would give nested dirs like skills/ttorch-review, and the
	// remediation hint below would then name the wrong root.
	subtrees := map[string]bool{}
	for _, src := range installed {
		rel := strings.TrimPrefix(src, embedRoot+"/")
		if top, _, nested := strings.Cut(rel, "/"); nested {
			subtrees[top] = true
		}
		if size, dims := review.Classify([]string{src}, 5, false, true); size == review.SizeDocsOnly {
			t.Errorf("installer writes %s into a session's directories, but the trust gate\n"+
				"classifies a diff of it as %s %v and drops the security reviewer.\n"+
				"Add its subtree to agentInstructionRoots in internal/review/size.go.", src, size, dims)
		}
	}
	// Guard against the walk or the routing silently collapsing: the payload has always had
	// several installed subtrees, and a single one would make the loop above near-vacuous.
	if len(subtrees) < 2 {
		t.Fatalf("expected the installer to route several subtrees, got %v", subtrees)
	}
	t.Logf("%d installed files across %d subtrees, none classified as inert prose", len(installed), len(subtrees))

	// The loop above only sees filenames the payload happens to carry today, and most of them
	// are rescued by their basename (SKILL.md, AGENTS.global.md) rather than by their subtree.
	// That would leave agentInstructionRoots barely tested, so probe each installed subtree
	// with a deliberately neutral filename that no basename rule can save.
	//
	// The requirement is on the SUBTREE, not on the route that reaches it. An earlier version
	// probed whether the neutral name was itself routed and skipped the subtree when it was
	// not, which exempted content/assets: only the exact path assets/AGENTS.global.md is
	// installed there, so the probe routed nowhere and the root went unenforced. Removing
	// {content, assets} then left this test green while content/assets/notes.md regressed to
	// prose. If the installer takes anything out of a subtree and puts it where a session
	// reads it, that subtree is an instruction subtree, however few of its files are routed.
	for sub := range subtrees {
		probeSrc := path.Join(embedRoot, sub, "notes.md")
		if size, dims := review.Classify([]string{probeSrc}, 5, false, true); size == review.SizeDocsOnly {
			t.Errorf("the installer writes files out of %s/%s into a session's directories, but\n"+
				"the trust gate classifies %s as %s %v and drops the security reviewer.\n"+
				"Add {%q, %q} to agentInstructionRoots in internal/review/size.go.",
				embedRoot, sub, probeSrc, size, dims, embedRoot, sub)
		}
	}
}
