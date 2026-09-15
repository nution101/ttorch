package brieflint

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// --- Rule 1: a target branch must be declared, and must exist on the remote -------------
//
// Briefs that never named a target produced work on the wrong base. Requiring the brief to
// name <remote>/<branch>, and verifying that it resolves, catches a typo or a deleted
// branch before a worker is dispatched onto it.

// target is a <remote>/<branch> reference as the brief wrote it.
type target struct {
	branch string
	quote  string
	line   int
}

// targetRe matches a <remote>/<branch> reference. Branch names may contain slashes
// ("ttorch/TTORCH-BRIEF-LINT"), so the branch is greedy and its trailing sentence
// punctuation is trimmed afterwards.
func targetRe(remote string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(remote) + `/([A-Za-z0-9._][A-Za-z0-9._/-]*)`)
}

// targets returns every distinct <remote>/<branch> the brief declares, in order of first
// mention.
func (b *brief) targets(remote string) []target {
	var out []target
	seen := map[string]bool{}
	for _, m := range targetRe(remote).FindAllStringSubmatchIndex(b.raw, -1) {
		branch := strings.TrimRight(b.raw[m[2]:m[3]], `.,;:)]}"'`+"`")
		if branch == "" || seen[branch] {
			continue
		}
		seen[branch] = true
		out = append(out, target{
			branch: branch,
			quote:  remote + "/" + branch,
			line:   b.lineAt(m[0]),
		})
	}
	return out
}

func checkTargetBranch(b *brief, opt Options) ([]Finding, []string) {
	remote := opt.remote()
	targets := b.targets(remote)
	if len(targets) == 0 {
		return []Finding{{
			Rule:   RuleTargetBranch,
			Status: StatusFail,
			Detail: fmt.Sprintf("the brief declares no target branch; name it explicitly as %s/<branch> so the worker cannot pick a base", remote),
		}}, nil
	}
	if opt.Repo == "" {
		var out []Finding
		for _, t := range targets {
			out = append(out, Finding{
				Rule:   RuleTargetBranch,
				Status: StatusIndeterminate,
				Detail: fmt.Sprintf("cannot verify %s exists: no repository was given to query the remote from", t.quote),
				Quote:  t.quote,
				Line:   t.line,
			})
		}
		return out, nil
	}
	var findings []Finding
	var notes []string
	for _, t := range targets {
		ok, err := opt.remoteBranchExists(remote, t.branch)
		switch {
		case err != nil:
			findings = append(findings, Finding{
				Rule:   RuleTargetBranch,
				Status: StatusIndeterminate,
				Detail: fmt.Sprintf("cannot verify %s exists: %v", t.quote, err),
				Quote:  t.quote,
				Line:   t.line,
			})
		case !ok:
			findings = append(findings, Finding{
				Rule:   RuleTargetBranch,
				Status: StatusFail,
				Detail: fmt.Sprintf("declared target %s does not exist on remote %s (typo, or the branch is gone)", t.quote, remote),
				Quote:  t.quote,
				Line:   t.line,
			})
		default:
			notes = append(notes, fmt.Sprintf("target-branch: %s exists on remote %s", t.quote, remote))
		}
	}
	return findings, notes
}

// --- Rule 2: every file path the brief cites must exist ---------------------------------
//
// A brief citing a path that is not on the target sends a worker hunting for a file that
// was never there.
//
// The ref a citation is resolved against is the crux. A citation WITHOUT a line number is
// about the work's base, so it is resolved at Options.Ref (the declared target by default).
// A file:line citation is about the commit it was READ at — typically a gate finding
// quoting a worker's HEAD, where the file is longer because the diff added to it. Resolving
// such a citation against the base false-positives on a citation that was perfectly valid
// at the reviewed commit, which is exactly the defect this rule is written to avoid. So a
// file:line citation is resolved at Options.CitationsRef, and when no such ref was supplied
// it is reported as INDETERMINATE — never assumed good, and never silently resolved against
// the base. Both refs are named in the report.

// citation is a path the brief cites, with the line it cited if any.
type citation struct {
	raw   string // as written, e.g. "internal/cli/cli.go:40"
	path  string
	line  int // 0 when the citation named no line
	quote string
	src   int // 1-based line in the brief
	// create is set when the citing sentence asks for the path to be CREATED, which
	// exempts it from the existence check (it is not supposed to exist yet).
	create bool
}

var (
	// urlRe strips URLs before scanning, so a link's path component is not mistaken for a
	// path in this repository.
	urlRe = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://\S+`)
	// candidateRe is a deliberately broad token match; classification happens in
	// citationOf, which rejects far more than it accepts.
	candidateRe = regexp.MustCompile(`[A-Za-z0-9_.~/*?:-]{2,}`)
	// extRe is the extension of a bare (slash-free) filename.
	extRe = regexp.MustCompile(`^[A-Za-z0-9_~-]+\.([A-Za-z0-9]{1,8})$`)
	// lineSuffixRe splits a trailing :<line> off a citation.
	lineSuffixRe = regexp.MustCompile(`^(.*?):(\d+)$`)
)

// bareFileExts are the extensions a slash-free token must carry to count as a cited file.
// Requiring a known extension is what keeps "e.g.", "i.e." and "1.2.3" out of the citation
// set; a path WITH a slash needs only a dotted last segment.
var bareFileExts = map[string]bool{
	"go": true, "mod": true, "sum": true, "md": true, "txt": true, "sh": true, "bash": true,
	"zsh": true, "json": true, "yaml": true, "yml": true, "toml": true, "ini": true,
	"cfg": true, "conf": true, "lock": true, "ts": true, "tsx": true, "js": true,
	"jsx": true, "py": true, "rb": true, "java": true, "kt": true, "kts": true, "rs": true,
	"c": true, "h": true, "cc": true, "cpp": true, "hpp": true, "m": true, "swift": true,
	"sql": true, "proto": true, "html": true, "css": true, "scss": true, "xml": true,
	"gradle": true, "tf": true, "dockerfile": true, "plist": true, "csv": true,
}

// citationOf classifies one token. ok is false for everything that is not a citable path.
// A citation is a token whose last segment carries an extension, or one written with a
// trailing slash (an explicit directory). A slash-free path-looking word must carry a known
// file extension. This is deliberately conservative: prose like "and/or" or "n/a", a glob
// like "./..." or "*.go", and a bare directory written without a trailing slash are all
// rejected rather than reported as missing files.
func citationOf(tok string) (citation, bool) {
	tok = strings.Trim(tok, "`\"'()[]{}<>,;")
	tok = strings.TrimRight(tok, ".")
	if tok == "" {
		return citation{}, false
	}
	c := citation{raw: tok, path: tok}
	if m := lineSuffixRe.FindStringSubmatch(tok); m != nil {
		n, err := strconv.Atoi(m[2])
		if err != nil || n <= 0 {
			return citation{}, false
		}
		c.path, c.line = m[1], n
	}
	p := c.path
	switch {
	case p == "" || strings.ContainsAny(p, "*?:"), strings.Contains(p, ".."):
		// A glob, a leftover colon, or a relative/elided path is not a citation.
		return citation{}, false
	case strings.HasSuffix(p, "/"):
		// An explicit directory citation; strip the slash for git.
		c.path = strings.TrimSuffix(p, "/")
		return c, c.path != "" && !strings.HasPrefix(c.path, "/")
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, "./"):
		return citation{}, false
	case strings.Contains(p, "/"):
		last := p[strings.LastIndexByte(p, '/')+1:]
		i := strings.LastIndexByte(last, '.')
		return c, i > 0 && i < len(last)-1
	default:
		m := extRe.FindStringSubmatch(p)
		return c, m != nil && bareFileExts[strings.ToLower(m[1])]
	}
}

// citations returns every distinct path the brief cites, in order of first mention. A
// reference to the remote itself ("origin/main") is a ref, not a path, and is excluded.
func (b *brief) citations(remote string) []citation {
	var out []citation
	seen := map[string]bool{}
	text := urlRe.ReplaceAllStringFunc(b.raw, func(s string) string { return strings.Repeat(" ", len(s)) })
	refPrefix := strings.ToLower(remote) + "/"
	for _, sent := range sentences(text) {
		create := createSignal.MatchString(sent.text)
		for _, m := range candidateRe.FindAllStringIndex(sent.text, -1) {
			tok := sent.text[m[0]:m[1]]
			if strings.HasPrefix(strings.ToLower(tok), refPrefix) {
				continue
			}
			c, ok := citationOf(tok)
			if !ok || seen[c.raw] {
				continue
			}
			seen[c.raw] = true
			c.quote = c.raw
			c.src = b.lineAt(sent.off + m[0])
			c.create = create
			out = append(out, c)
		}
	}
	return out
}

func checkFilePaths(b *brief, opt Options) ([]Finding, []string) {
	cites := b.citations(opt.remote())
	if len(cites) == 0 {
		return nil, []string{"file-paths: the brief cites no file paths"}
	}
	if opt.Repo == "" {
		return []Finding{{
			Rule:   RuleFilePaths,
			Status: StatusIndeterminate,
			Detail: fmt.Sprintf("cannot resolve the %d cited path(s): no repository was given", len(cites)),
		}}, nil
	}
	// The base ref: an explicit --ref wins, else the target branch the brief itself
	// declares. Naming it in the report is part of the rule — a citation is only meaningful
	// against a stated ref.
	ref := opt.Ref
	if ref == "" {
		if t := b.targets(opt.remote()); len(t) > 0 {
			ref = opt.remote() + "/" + t[0].branch
		}
	}
	if ref == "" {
		return []Finding{{
			Rule:   RuleFilePaths,
			Status: StatusIndeterminate,
			Detail: fmt.Sprintf("cannot resolve the %d cited path(s): the brief declares no target branch and no explicit ref was given", len(cites)),
		}}, nil
	}
	if ok, err := opt.revExists(ref); err != nil {
		return []Finding{{
			Rule:   RuleFilePaths,
			Status: StatusIndeterminate,
			Detail: fmt.Sprintf("cannot resolve cited paths at %s: %v", ref, err),
		}}, nil
	} else if !ok {
		return []Finding{{
			Rule:   RuleFilePaths,
			Status: StatusIndeterminate,
			Detail: fmt.Sprintf("cannot resolve cited paths: ref %s does not resolve in %s (fetch it, or pass an explicit ref)", ref, opt.Repo),
		}}, nil
	}
	notes := []string{fmt.Sprintf("file-paths: cited paths resolved at %s", ref)}
	var findings []Finding
	var withLine, created []citation
	for _, c := range cites {
		if c.create {
			created = append(created, c)
			continue
		}
		if c.line > 0 {
			withLine = append(withLine, c)
			continue
		}
		_, ok, err := opt.objectType(ref, c.path)
		switch {
		case err != nil:
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cannot check %s at %s: %v", c.path, ref, err),
			})
		case !ok:
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: StatusFail, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cited path %s does not exist at %s", c.path, ref),
			})
		}
	}
	if len(withLine) > 0 {
		findings = append(findings, opt.checkLineCitations(withLine, ref)...)
		if opt.CitationsRef != "" {
			notes = append(notes, fmt.Sprintf("file-paths: file:line citations resolved at %s", opt.CitationsRef))
		}
	}
	if len(created) > 0 {
		// Visible, never silent: an exemption the reader can see is one they can question.
		notes = append(notes, fmt.Sprintf("file-paths: %d citation(s) not checked, the brief asks for them to be created: %s",
			len(created), strings.Join(rawPaths(created), ", ")))
	}
	return findings, notes
}

func rawPaths(cites []citation) []string {
	out := make([]string, len(cites))
	for i, c := range cites {
		out[i] = c.raw
	}
	return out
}

// checkLineCitations resolves the file:line citations against Options.CitationsRef — the
// commit the citation is about. With no such ref the whole set is unevaluable and is
// reported as one finding naming it: guessing the base is what false-positived on a
// citation that was valid at the reviewed commit.
func (o Options) checkLineCitations(cites []citation, baseRef string) []Finding {
	if o.CitationsRef == "" {
		return []Finding{{
			Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: cites[0].raw, Line: cites[0].src,
			Detail: fmt.Sprintf("%d file:line citation(s) could not be resolved (%s): a line number is about the commit it was read at, not the base %s, and no citations ref was given. Pass the reviewed commit (a worker HEAD or sha) as the citations ref",
				len(cites), strings.Join(rawPaths(cites), ", "), baseRef),
		}}
	}
	if ok, err := o.revExists(o.CitationsRef); err != nil {
		return []Finding{{
			Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: cites[0].raw, Line: cites[0].src,
			Detail: fmt.Sprintf("cannot resolve file:line citations at %s: %v", o.CitationsRef, err),
		}}
	} else if !ok {
		return []Finding{{
			Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: cites[0].raw, Line: cites[0].src,
			Detail: fmt.Sprintf("cannot resolve file:line citations: citations ref %s does not resolve in %s", o.CitationsRef, o.Repo),
		}}
	}
	var findings []Finding
	for _, c := range cites {
		typ, ok, err := o.objectType(o.CitationsRef, c.path)
		switch {
		case err != nil:
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cannot check %s at %s: %v", c.path, o.CitationsRef, err),
			})
			continue
		case !ok:
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: StatusFail, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cited path %s does not exist at %s", c.path, o.CitationsRef),
			})
			continue
		case typ != "blob":
			// A directory cited with a line number: the path is real, and there is no line
			// to bound. Nothing further to check.
			continue
		}
		n, err := o.lineCount(o.CitationsRef, c.path)
		if err != nil {
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cannot count the lines of %s at %s: %v", c.path, o.CitationsRef, err),
			})
			continue
		}
		if c.line > n {
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: StatusFail, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cites line %d but %s has %d line(s) at %s", c.line, c.path, n, o.CitationsRef),
			})
		}
	}
	return findings
}

// --- Rule 3: hard counts need a verify-yourself hedge -----------------------------------
//
// A brief stating "there are N occurrences" makes a worker fix exactly N and stop. Counts
// in briefs have been wrong in practice. A brief that states a hard count must also tell
// the worker to verify it and report their own number. See signals.go for why the hedge is
// matched as decomposed word-stem signals rather than as accepted phrasings.

// countNouns are the nouns that turn a bare number into a countable claim about the work.
const countNouns = `occurrences?|instances?|places?|call[ -]?sites?|sites?|files?|tests?|cases?|matches?|usages?|uses|references?|refs?|hits?|callers?|functions?|methods?|packages?|modules?|errors?|warnings?|violations?|findings?|todos?|duplicates?|copies|copy`

var (
	// countNounRe matches "21 occurrences", "19 other files", "3 remaining call sites".
	countNounRe = regexp.MustCompile(`(?i)\b(\d+)\s+(?:\w+\s+){0,2}?(?:` + countNouns + `)\b`)
	// countPhraseRe matches a hard count stated without a countable noun: "all 19",
	// "exactly 21", "there are 19".
	countPhraseRe = regexp.MustCompile(`(?i)\b(?:all|exactly|precisely|there\s+are|there\s+were|i\s+counted|i\s+count)\s+(\d+)\b`)
)

// minHardCount is the smallest number treated as a hard count. "1 file" is overwhelmingly
// incidental prose; the failure this rule exists for needs a plural claim to bound.
const minHardCount = 2

// hardCounts returns the sentences that state a hard count, in order.
func hardCounts(b *brief) []span {
	var out []span
	seen := map[int]bool{}
	for _, s := range sentences(b.raw) {
		for _, re := range []*regexp.Regexp{countNounRe, countPhraseRe} {
			m := re.FindStringSubmatch(s.text)
			if m == nil {
				continue
			}
			if n, err := strconv.Atoi(m[1]); err != nil || n < minHardCount {
				continue
			}
			if !seen[s.off] {
				seen[s.off] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func checkHardCounts(b *brief, _ Options) ([]Finding, []string) {
	counts := hardCounts(b)
	if len(counts) == 0 {
		return nil, []string{"hard-counts: the brief states no hard count"}
	}
	if hedged(b) {
		return nil, []string{fmt.Sprintf("hard-counts: %d hard count(s), hedged", len(counts))}
	}
	var findings []Finding
	for _, s := range counts {
		findings = append(findings, Finding{
			Rule: RuleHardCounts, Status: StatusFail, Quote: s.trim(), Line: b.lineAt(s.off),
			Detail: "states a hard count with no verify-yourself hedge; tell the worker the figure may be wrong, to check it, and to report their own number",
		})
	}
	return findings, nil
}

// --- Rule 4: a bare prohibition that also bans required machinery -----------------------
//
// "Do NOT push" reads as banning the push a worker may legitimately need, so a brief must
// state the actual invariant (for example: no changes to a product branch, no PR) AND the
// allowed end state. A bare blanket prohibition has caused workers to stall, unsure whether
// they were permitted to finish.

var prohibitionRe = regexp.MustCompile(`(?i)\b(?:do\s+not|do\s+n[o']t|don'?t|never|no|avoid|under\s+no\s+circumstances)\s+(?:ever\s+)?(?:push(?:es|ing)?|merg(?:e|es|ing)|commit(?:s|ting)?|land(?:s|ing)?|deliver(?:s|ing)?|publish(?:es|ing)?|open(?:ing)?\s+(?:a|any)\s+pr)\b|\b(?:push|merge|commit|land)\s+nothing\b`)

// prohibitions returns the sentences carrying a prohibition on the machinery a worker may
// legitimately need, in order.
func prohibitions(b *brief) []span {
	var out []span
	for _, s := range sentences(b.raw) {
		if prohibitionRe.MatchString(s.text) {
			out = append(out, s)
		}
	}
	return out
}

func checkProhibition(b *brief, _ Options) ([]Finding, []string) {
	bans := prohibitions(b)
	if len(bans) == 0 {
		return nil, []string{"prohibition: the brief states no prohibition on push/merge/commit/PR"}
	}
	var bare []span
	bounded := false
	for _, s := range bans {
		if boundSignal.MatchString(s.text) {
			bounded = true
			continue
		}
		bare = append(bare, s)
	}
	var findings []Finding
	if !bounded {
		for _, s := range bare {
			findings = append(findings, Finding{
				Rule: RuleProhibition, Status: StatusFail, Quote: s.trim(), Line: b.lineAt(s.off),
				Detail: "blanket prohibition: state the invariant it protects (for example: no changes to a product branch, no PR) rather than banning the machinery outright",
			})
		}
	}
	if !endStateSignal.MatchString(b.raw) {
		findings = append(findings, Finding{
			Rule: RuleProhibition, Status: StatusFail, Quote: bans[0].trim(), Line: b.lineAt(bans[0].off),
			Detail: "prohibits delivery machinery but never states the allowed end state; say where the work is meant to stop (for example: commit on your own branch, leave the worktree clean, report the sha, and wait)",
		})
	}
	if len(findings) == 0 {
		return nil, []string{fmt.Sprintf("prohibition: %d prohibition(s), each bounded, with an allowed end state", len(bans))}
	}
	return findings, nil
}

// --- Rule 5: a standards reference, configurable per project ----------------------------
//
// Workers cannot follow rules they are not given, so a brief should point at the standards
// the project expects. WHAT that pointer is belongs to the project: the prototype hardcoded
// a grep for one project's corpus path, so a brief for any other repository failed the rule
// however correctly it stated its standards. See config.go for the declaration, and
// Config.StandardsEmpty for why a declared-but-empty value is unevaluable rather than a
// pass.

// standardsSignal is the fallback for a project that declares no pointer: the brief must
// point at standards SOMEHOW. Each stem is independently sufficient, and none is a required
// path, so no project is held to another project's layout. Stems stay narrow enough to mean
// "standards": a brief that merely cites a file under docs/ has named a file, not a
// convention, and a project that wants docs/ to count says so with brief-standards.
var standardsSignal = stemRe(
	"standard*", "convention*", "style guide", "styleguide", "house style", "code style",
	"coding style", "guideline*", "match the existing", "match the style",
	"match the shape", "match the surrounding", "contributing*", "agents.md", "claude.md",
	"lint rules", "best practice*", "idiom*",
)

func checkStandards(b *brief, opt Options) ([]Finding, []string) {
	cfg := opt.Config
	if cfg.StandardsEmpty {
		return []Finding{{
			Rule: RuleStandards, Status: StatusIndeterminate,
			Detail: fmt.Sprintf("cannot evaluate: %s is declared with no value in %s. A project that opts in must name the pointer a brief has to cite; an empty value can never be satisfied", standardsKey, cfg.Source),
		}}, nil
	}
	if len(cfg.Standards) > 0 {
		for _, p := range cfg.Standards {
			if strings.Contains(b.lower, strings.ToLower(p)) {
				return nil, []string{fmt.Sprintf("standards: the brief cites the project's declared pointer %q", p)}
			}
		}
		return []Finding{{
			Rule: RuleStandards, Status: StatusFail,
			Detail: fmt.Sprintf("the brief cites none of the standards pointer(s) this project declares (%s, from %s %s); point the worker at them", strings.Join(cfg.Standards, ", "), cfg.Source, standardsKey),
		}}, nil
	}
	notes := []string{fmt.Sprintf("standards: no project pointer declared (%s), so any explicit standards reference is accepted", standardsKey)}
	if standardsSignal.MatchString(b.raw) {
		return nil, notes
	}
	return []Finding{{
		Rule: RuleStandards, Status: StatusFail,
		Detail: fmt.Sprintf("the brief points at no standards; say which conventions the work must follow, or declare the project's pointer as `%s <pointer>` in %s", standardsKey, configFile),
	}}, notes
}
