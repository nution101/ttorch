package brieflint

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
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

// maxRemoteTargets caps how many distinct targets one brief has verified over the network.
// Rule 1 queries the remote once per distinct <remote>/<branch> token it finds in prose, so
// without a cap the brief decides how many authenticated requests its reader fires and how
// long the reader waits. An honest brief names one target, occasionally two; anything past
// the cap is reported as unchecked rather than either trusted or queried.
const maxRemoteTargets = 3

func checkTargetBranch(ctx context.Context, b *brief, opt Options) ([]Finding, []string) {
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
	if len(targets) > maxRemoteTargets {
		// Over the cap the surplus is neither queried nor trusted: it is named as
		// unchecked, so the reader can see exactly what was not verified.
		surplus := targets[maxRemoteTargets:]
		targets = targets[:maxRemoteTargets]
		findings = append(findings, Finding{
			Rule:   RuleTargetBranch,
			Status: StatusIndeterminate,
			Detail: fmt.Sprintf("the brief names %d more %s/<branch> target(s) than the %d this rule verifies over the network (%s); name one target, or disable this rule for a brief that legitimately names many",
				len(surplus), remote, maxRemoteTargets, joinCapped(targetNames(surplus), namesInDetail)),
			Quote: surplus[0].quote,
			Line:  surplus[0].line,
		})
	}
	for _, t := range targets {
		ok, err := opt.remoteBranchExists(ctx, remote, t.branch)
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

// namesInDetail caps how many names a finding spells out. The reader needs enough to
// recognize what went unchecked; a finding that prints forty branch names buries every other
// finding in the report.
const namesInDetail = 5

// joinCapped lists at most max names, then says how many more there were.
func joinCapped(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:max], ", "), len(names)-max)
}

func targetNames(targets []target) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.quote
	}
	return out
}

// --- Rule 2: every file path the brief cites must exist ---------------------------------
//
// A brief citing a path that is not on the target sends a worker hunting for a file that
// was never there.
//
// The ref a citation resolves against is the crux, because the two kinds of citation are
// about different commits:
//
//   - A citation WITHOUT a line number is about the work's base, and resolves at
//     Options.Ref (the target branch the brief declares, by default).
//   - A file:line citation is about the commit it was READ at, which is typically a gate
//     finding quoting a worker's HEAD, where the file is longer because the diff added to
//     it. Options.CitationsRef names that commit.
//
// When the caller NAMES the citations ref, the answer is authoritative: a missing path or a
// line past end-of-file is a violation. When the caller does NOT name it, the citation is
// still checked, against the base, because that is the only commit at hand and because
// pointing a worker at a line of existing code is the most ordinary thing a brief does and
// must not require a flag. But there a miss is downgraded to StatusIndeterminate: a line
// past end-of-file on the base may perfectly well exist at the commit the citation was read
// at, and calling that a violation is exactly the false positive this rule exists to avoid.
//
// So a citation that resolves is a pass either way, a citation that does not is never
// silently accepted, and the report always names the ref used and whether it was given or
// defaulted.

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
// maxPathBytes is the longest token treated as a path. No filesystem accepts more, so a
// longer one is prose that happens to contain slashes, and resolving it would spend git
// work on text that cannot name a file.
const maxPathBytes = 4096

// maxTokenBytes caps a cited token on its way into a finding. A 60 KB token produced 121 KB
// of terminal output, and every other rule already quotes something bounded.
const maxTokenBytes = 120

// clipToken renders a cited token for the terminal, capped.
func clipToken(tok string) string {
	if len(tok) <= maxTokenBytes {
		return tok
	}
	cut := maxTokenBytes
	for cut > 0 && !utf8.RuneStart(tok[cut]) {
		cut--
	}
	return tok[:cut] + "..."
}

func citationOf(tok string) (citation, bool) {
	if len(tok) > maxPathBytes {
		return citation{}, false
	}
	tok = strings.Trim(tok, "`\"'()[]{}<>,;")
	// A trailing dot or colon is punctuation: "see dev/report.md: the rule set" cites
	// dev/report.md. A colon that introduces a LINE keeps its digits, so :40 survives this
	// and is split off below.
	tok = strings.TrimRight(tok, ".:")
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
func (b *brief) citations(ctx context.Context, remote string) ([]citation, bool) {
	var out []citation
	seen := map[string]int{} // raw citation -> index in out
	text := urlRe.ReplaceAllStringFunc(b.raw, func(s string) string { return strings.Repeat(" ", len(s)) })
	refPrefix := strings.ToLower(remote) + "/"
	scanned := 0
	for _, sent := range sentences(text) {
		// Once per sentence, not once per candidate in it.
		verbs := createVerbs(sent.text)
		for _, m := range candidateRe.FindAllStringIndex(sent.text, -1) {
			if scanned%budgetCheckEvery == 0 && ctx.Err() != nil {
				return out, false
			}
			scanned++
			tok := sent.text[m[0]:m[1]]
			if strings.HasPrefix(strings.ToLower(tok), refPrefix) {
				continue
			}
			c, ok := citationOf(tok)
			if !ok {
				continue
			}
			c.quote = clipToken(c.raw)
			c.src = b.lineAt(sent.off + m[0])
			// The exemption is earned by THIS mention, from a create verb governing this
			// path here, not by the sentence and not by the path.
			c.create = governedByCreate(sent.text, verbs, m[0])
			if i, ok := seen[c.raw]; ok {
				// A later mention that earned no exemption puts the path back under the
				// check. Otherwise one "Add internal/x/y.go" near the top would exempt
				// every later reference to that path in the brief.
				if out[i].create && !c.create {
					out[i] = c
				}
				continue
			}
			seen[c.raw] = len(out)
			out = append(out, c)
		}
	}
	return out, true
}

// maxCitations caps how many distinct cited paths one brief has resolved. Each costs local
// git work (and a line count reads the blob), so without a cap the brief decides how much
// work its reader does. Past the cap the surplus is named as unchecked, never passed.
const maxCitations = 64

func checkFilePaths(ctx context.Context, b *brief, opt Options) ([]Finding, []string) {
	cites, complete := b.citations(ctx, opt.remote())
	if !complete {
		return []Finding{budgetFinding(RuleFilePaths)}, nil
	}
	if len(cites) == 0 {
		return nil, []string{"file-paths: no file path was recognised in the brief (a path is recognised by its shape: a slash, or a bare name with a known extension)"}
	}
	if opt.Repo == "" {
		return []Finding{{
			Rule:   RuleFilePaths,
			Status: StatusIndeterminate,
			Detail: fmt.Sprintf("cannot resolve the %d cited path(s): no repository was given", len(cites)),
		}}, nil
	}
	// The base ref: an explicit Ref wins, else the target branch the brief itself declares.
	// Naming it in the report is part of the rule — a citation is only meaningful against a
	// stated ref.
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
	if ok, err := opt.revExists(ctx, ref); err != nil {
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
	// "resolved against", not "resolved at": this is printed before anything is checked, so
	// it says which ref the question was put to, not that the answer was yes. It used to sit
	// next to a finding reporting that a path did not resolve there.
	notes := []string{fmt.Sprintf("file-paths: cited paths resolved against %s", ref)}
	var findings []Finding
	if len(cites) > maxCitations {
		surplus := cites[maxCitations:]
		cites = cites[:maxCitations]
		findings = append(findings, Finding{
			Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: surplus[0].quote, Line: surplus[0].src,
			Detail: fmt.Sprintf("%d cited path(s) past the first %d were not checked (from %s on); a brief citing this many paths is doing too much, so split it or disable this rule",
				len(surplus), maxCitations, clipToken(surplus[0].raw)),
		})
	}
	var withLine, created []citation
	for _, c := range cites {
		if c.create {
			created = append(created, c)
			// An exempted citation skips the EXISTENCE check, not the line bound. If it
			// names a line and the file turns out to exist, the line is still checked
			// against it; only a missing file is forgiven. Skipping both let
			// "write dev/report.md:9999" past unexamined.
			if c.line == 0 {
				continue
			}
		}
		if c.line > 0 {
			withLine = append(withLine, c)
			continue
		}
		_, ok, err := opt.objectType(ctx, ref, c.path)
		switch {
		case err != nil:
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cannot check %s at %s: %v", clipToken(c.path), ref, err),
			})
		case !ok:
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: StatusFail, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cited path %s does not exist at %s", clipToken(c.path), ref),
			})
		}
	}
	if len(withLine) > 0 {
		lineRef, named := opt.lineCitationRef(ref)
		findings = append(findings, opt.checkLineCitations(ctx, withLine, lineRef, named)...)
		notes = append(notes, fmt.Sprintf("file-paths: file:line citations resolved at %s (%s)", lineRef, refSource(named)))
	}
	if len(created) > 0 {
		// Visible, never silent: an exemption the reader can see is one they can question.
		notes = append(notes, fmt.Sprintf("file-paths: %d citation(s) exempt from the existence check, read as asking for them to be created (wording only: a create verb governing the mention): %s",
			len(created), strings.Join(rawPaths(created), ", ")))
	}
	return findings, notes
}

// lineCitationRef returns the ref a file:line citation resolves against, and whether the
// caller named it. Defaulting to the base ref is what keeps the ordinary case — a brief
// pointing at a line of existing code — a plain pass with no flag; the bool is what stops
// that default from turning an ambiguous miss into a false violation.
func (o Options) lineCitationRef(baseRef string) (string, bool) {
	if o.CitationsRef != "" {
		return o.CitationsRef, true
	}
	return baseRef, false
}

func refSource(named bool) string {
	if named {
		return "given"
	}
	return "defaulted to the base; an unresolved citation there is unevaluable, not a failure"
}

func rawPaths(cites []citation) []string {
	out := make([]string, len(cites))
	for i, c := range cites {
		out[i] = clipToken(c.raw)
	}
	return out
}

// checkLineCitations resolves file:line citations at ref. named says whether the caller
// supplied that ref: if they did, a miss is a violation, because they have told us which
// commit the citation is about. If it defaulted to the base, a miss is unevaluable and the
// finding says how to resolve it — the file may legitimately be longer, or newer, at the
// commit the citation was read at.
//
// Line counts are memoized per path, so a brief citing twenty lines of one file reads that
// file once rather than twenty times.
func (o Options) checkLineCitations(ctx context.Context, cites []citation, ref string, named bool) []Finding {
	miss, remedy := StatusFail, ""
	if !named {
		miss = StatusIndeterminate
		remedy = "; it may exist at the commit the citation was read at, so pass that commit (a worker HEAD or a reviewed sha) as the citations ref to resolve this"
	}
	// The base ref was already verified by the caller; only a ref the caller named needs
	// checking here.
	if named {
		if ok, err := o.revExists(ctx, ref); err != nil {
			return []Finding{{
				Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: cites[0].raw, Line: cites[0].src,
				Detail: fmt.Sprintf("cannot resolve file:line citations at %s: %v", ref, err),
			}}
		} else if !ok {
			return []Finding{{
				Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: cites[0].raw, Line: cites[0].src,
				Detail: fmt.Sprintf("cannot resolve file:line citations: citations ref %s does not resolve in %s", ref, o.Repo),
			}}
		}
	}
	var findings []Finding
	counted := map[string]int{}
	for _, c := range cites {
		typ, ok, err := o.objectType(ctx, ref, c.path)
		switch {
		case err != nil:
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cannot check %s at %s: %v", clipToken(c.path), ref, err),
			})
			continue
		case !ok:
			if c.create {
				// The brief asked for this file; not existing yet is the point.
				continue
			}
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: miss, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cited path %s does not exist at %s%s", clipToken(c.path), ref, remedy),
			})
			continue
		case typ != "blob":
			// A directory cited with a line number: the path is real, and there is no line
			// to bound. Nothing further to check.
			continue
		}
		n, ok := counted[c.path]
		if !ok {
			n, err = o.lineCount(ctx, ref, c.path)
			if err != nil {
				findings = append(findings, Finding{
					Rule: RuleFilePaths, Status: StatusIndeterminate, Quote: c.quote, Line: c.src,
					Detail: fmt.Sprintf("cannot count the lines of %s at %s: %v", clipToken(c.path), ref, err),
				})
				continue
			}
			counted[c.path] = n
		}
		if c.line > n {
			findings = append(findings, Finding{
				Rule: RuleFilePaths, Status: miss, Quote: c.quote, Line: c.src,
				Detail: fmt.Sprintf("cites line %d but %s has %d line(s) at %s%s", c.line, clipToken(c.path), n, ref, remedy),
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
//
// What this rule can establish is that hedge WORDING sits near the count. It cannot
// establish that the brief hedges. "I verified the count myself, so do not re-count it"
// carries every stem a hedge carries and means the opposite, and no vocabulary fixes that,
// because the words are the same words. Everything this rule prints says wording, not
// meaning, so a reader is not told the brief hedges when what was checked is that it could
// be read as hedging.

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

// countHit is one hard count: which span states it, the sentence inside that span carrying
// it, and where the number sits.
type countHit struct {
	sent int    // index into sents
	unit [2]int // the sentence carrying the count, relative to the span
	loc  [2]int // the number itself, relative to the span
}

// hardCounts returns every hard count in the brief, one per number stated, not one per span.
// A span holds more than one sentence whenever the splitter merged them, and counting spans
// meant "there are 21 occurrences. there are 19 files." certified one hard count in
// lowercase and two capitalised.
func hardCounts(ctx context.Context, sents []span) ([]countHit, bool) {
	var out []countHit
	scanned := 0
	for i, s := range sents {
		if scanned%budgetCheckEvery == 0 && ctx.Err() != nil {
			return out, false
		}
		scanned++
		seen := map[int]bool{} // the number's offset: both patterns can match one count
		for _, re := range []*regexp.Regexp{countNounRe, countPhraseRe} {
			for _, m := range re.FindAllStringSubmatchIndex(s.text, -1) {
				num := s.text[m[2]:m[3]]
				if n, err := strconv.Atoi(num); err != nil || n < minHardCount {
					continue
				}
				if seen[m[2]] {
					continue
				}
				seen[m[2]] = true
				scanned++
				out = append(out, countHit{sent: i, unit: s.unitAround(m[2]), loc: [2]int{m[2], m[3]}})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].sent != out[b].sent {
			return out[a].sent < out[b].sent
		}
		return out[a].loc[0] < out[b].loc[0]
	})
	return out, true
}

func checkHardCounts(ctx context.Context, b *brief, _ Options) ([]Finding, []string) {
	sents := sentences(b.raw)
	counts, complete := hardCounts(ctx, sents)
	if !complete {
		return []Finding{budgetFinding(RuleHardCounts)}, nil
	}
	if len(counts) == 0 {
		return nil, []string{"hard-counts: no hard count was recognised in the brief"}
	}
	hedged := hedgedBlocks(sents)
	asksForTheNumber := reportsTheNumber(b)
	var findings []Finding
	reported := map[[2]int]bool{} // one finding per sentence, not per count inside it
	for n, c := range counts {
		if n%budgetCheckEvery == 0 && ctx.Err() != nil {
			return append(findings, budgetFinding(RuleHardCounts)), nil
		}
		s := sents[c.sent]
		var detail string
		switch {
		case !hedgedAt(hedged, s.blk):
			detail = "states a hard count with no hedge wording near it; in this paragraph or list item, or the next one, say the figure may be wrong or tell the worker to count it themselves"
		case !asksForTheNumber:
			detail = "hedges the count but never asks for the worker's own number; tell them to report the figure they actually find"
		default:
			continue
		}
		key := [2]int{c.sent, c.unit[0]}
		if reported[key] {
			continue
		}
		reported[key] = true
		unit := span{text: s.text[c.unit[0]:c.unit[1]], off: s.off + c.unit[0]}
		findings = append(findings, Finding{
			Rule: RuleHardCounts, Status: StatusFail, Quote: unit.trim(), Line: b.lineAt(unit.off),
			Detail: detail,
		})
	}
	if len(findings) == 0 {
		return nil, []string{fmt.Sprintf("hard-counts: %d hard count(s), each with hedge wording in reach, and a reporting phrase somewhere in the brief (wording only: the check cannot tell a hedge from a sentence forbidding one, and the reporting phrase need not be about this count)", len(counts))}
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

func checkProhibition(ctx context.Context, b *brief, _ Options) ([]Finding, []string) {
	spans := prohibitions(b)
	if len(spans) == 0 {
		return nil, []string{"prohibition: no prohibition on push/merge/commit/PR was recognised in the brief"}
	}
	var findings []Finding
	banCount := 0
	var first span // the sentence carrying the first ban, for the end-state finding
	for _, s := range spans {
		hits, complete := prohibitionHits(ctx, s)
		if !complete {
			return append(findings, budgetFinding(RuleProhibition)), nil
		}
		reported := map[int]bool{} // one finding per sentence, not per ban inside it
		for _, hit := range hits {
			banCount++
			unit := span{text: s.text[hit.unit[0]:hit.unit[1]], off: s.off + hit.unit[0]}
			if first.text == "" {
				first = unit
			}
			if hit.bounded || reported[hit.unit[0]] {
				continue
			}
			reported[hit.unit[0]] = true
			findings = append(findings, Finding{
				Rule: RuleProhibition, Status: StatusFail, Quote: unit.trim(), Line: b.lineAt(unit.off),
				Detail: fmt.Sprintf("blanket prohibition %q: state the invariant it protects (for example: no changes to a product branch, no PR) rather than banning the machinery outright", strings.TrimSpace(s.text[hit.loc[0]:hit.loc[1]])),
			})
		}
	}
	if !endStateSignal.MatchString(b.raw) {
		findings = append(findings, Finding{
			Rule: RuleProhibition, Status: StatusFail, Quote: first.trim(), Line: b.lineAt(first.off),
			Detail: "prohibits delivery machinery but never states the allowed end state; say where the work is meant to stop (for example: commit on your own branch, leave the worktree clean, report the sha, and wait)",
		})
	}
	if len(findings) == 0 {
		return nil, []string{fmt.Sprintf("prohibition: %d prohibition(s), each with bounding wording in reach of the ban (its own clause, or a clause at the edge of the sentence that opens with the bound), and an allowed end state stated somewhere in the brief (wording only: the check cannot tell whether the bound it found actually limits the ban)", banCount)}
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

func checkStandards(ctx context.Context, b *brief, opt Options) ([]Finding, []string) {
	if ctx.Err() != nil {
		return []Finding{budgetFinding(RuleStandards)}, nil
	}
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
				return nil, []string{fmt.Sprintf("standards: the brief mentions the project's declared pointer %q (wording only: a mention, which the check cannot tell from one telling the worker to ignore it)", p)}
			}
		}
		return []Finding{{
			Rule: RuleStandards, Status: StatusFail,
			Detail: fmt.Sprintf("the brief cites none of the standards pointer(s) this project declares (%s, from %s %s); point the worker at them", strings.Join(quoteAll(cfg.Standards), ", "), cfg.Source, standardsKey),
		}}, nil
	}
	notes := []string{fmt.Sprintf("standards: no project pointer declared (%s), so any explicit standards reference is accepted (wording only: a standards-shaped phrase, which the check cannot tell from one telling the worker to ignore the conventions)", standardsKey)}
	if standardsSignal.MatchString(b.raw) {
		return nil, notes
	}
	return []Finding{{
		Rule: RuleStandards, Status: StatusFail,
		Detail: fmt.Sprintf("the brief points at no standards; say which conventions the work must follow, or declare the project's pointer as `%s <pointer>` in %s", standardsKey, configFile),
	}}, notes
}
