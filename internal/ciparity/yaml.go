package ciparity

import "strings"

// This file implements a small, dependency-free parser for the subset of YAML used by
// GitHub Actions workflow files: block mappings, block sequences, plain/quoted scalars,
// and block scalars (| and >). It is deliberately NOT a general YAML parser — it exists
// only to locate jobs -> steps -> {name, run, uses, shell, if, ...} in a workflow
// document. Flow collections ([a, b] / {a: b}) are preserved verbatim as opaque scalar
// strings because the extractor never needs to descend into them.

// node is one of: *mapping, []node, or string (a scalar).
type node interface{}

// mapping is an insertion-ordered string-keyed map, so jobs and steps report in document
// order — which keeps extraction deterministic and unit tests stable.
type mapping struct {
	keys   []string
	values map[string]node
}

func newMapping() *mapping {
	return &mapping{values: map[string]node{}}
}

func (m *mapping) set(k string, v node) {
	if _, ok := m.values[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.values[k] = v
}

func (m *mapping) get(k string) (node, bool) {
	v, ok := m.values[k]
	return v, ok
}

// pline is one physical input line, split into its indentation and content.
type pline struct {
	indent int    // count of leading spaces
	body   string // line with leading indent removed and trailing whitespace trimmed
	raw    string // the full line (no trailing newline); used for block-scalar content
}

func splitLines(data string) []pline {
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	var out []pline
	for _, raw := range strings.Split(data, "\n") {
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		out = append(out, pline{
			indent: indent,
			body:   strings.TrimRight(raw[indent:], " \t"),
			raw:    raw,
		})
	}
	return out
}

type parser struct {
	lines []pline
	i     int
}

// parseYAML parses a single YAML document into a node tree. It is intentionally lenient:
// malformed regions are skipped rather than treated as fatal, since the goal is to
// recover the well-formed jobs/steps structure even from an unusual workflow file.
func parseYAML(data string) node {
	p := &parser{lines: splitLines(data)}
	p.skipInsignificant()
	// Skip a leading document-start marker if present.
	if p.i < len(p.lines) && p.lines[p.i].indent == 0 && p.lines[p.i].body == "---" {
		p.i++
	}
	return p.parseBlock(0)
}

func (p *parser) skipInsignificant() {
	for p.i < len(p.lines) {
		b := p.lines[p.i].body
		if b == "" || strings.HasPrefix(b, "#") {
			p.i++
			continue
		}
		return
	}
}

// cur returns the current significant line without consuming it. A document-end or
// second document-start marker terminates the current document.
func (p *parser) cur() (pline, bool) {
	p.skipInsignificant()
	if p.i >= len(p.lines) {
		return pline{}, false
	}
	ln := p.lines[p.i]
	if ln.indent == 0 && (ln.body == "---" || ln.body == "...") {
		return pline{}, false
	}
	return ln, true
}

// parseBlock parses the collection or scalar whose first entry sits at exactly indent.
func (p *parser) parseBlock(indent int) node {
	ln, ok := p.cur()
	if !ok || ln.indent != indent {
		return nil
	}
	if ln.body == "-" || strings.HasPrefix(ln.body, "- ") {
		return p.parseSequence(indent)
	}
	if keyColon(ln.body) < 0 {
		// A bare scalar on its own line (e.g. a sequence item value).
		if ind, ok := blockIndicator(ln.body); ok {
			p.i++
			return p.parseBlockScalar(indent, ind)
		}
		p.i++
		return parseScalar(stripInlineComment(ln.body))
	}
	return p.parseMapping(indent)
}

func (p *parser) parseMapping(indent int) node {
	m := newMapping()
	for {
		ln, ok := p.cur()
		if !ok || ln.indent != indent {
			break
		}
		if ln.body == "-" || strings.HasPrefix(ln.body, "- ") {
			break // a dash at this indent is not a mapping entry
		}
		key, val, hasInline, blockInd := splitKey(ln.body)
		if key == "" {
			break // not a recognizable mapping line; stop defensively
		}
		p.i++ // consume the key line
		switch {
		case blockInd != "":
			m.set(key, p.parseBlockScalar(indent, blockInd))
		case hasInline:
			m.set(key, parseScalar(val))
		default:
			m.set(key, p.parseNestedValue(indent))
		}
	}
	return m
}

// parseNestedValue parses the value of a "key:" line that had no inline value: a mapping
// or sequence indented deeper than the key, or a sequence at the same indent as the key
// (YAML permits both for block sequences).
func (p *parser) parseNestedValue(parentIndent int) node {
	ln, ok := p.cur()
	if !ok {
		return nil
	}
	if ln.indent > parentIndent {
		return p.parseBlock(ln.indent)
	}
	if ln.indent == parentIndent && (ln.body == "-" || strings.HasPrefix(ln.body, "- ")) {
		return p.parseSequence(parentIndent)
	}
	return nil
}

func (p *parser) parseSequence(indent int) node {
	seq := []node{}
	for {
		ln, ok := p.cur()
		if !ok || ln.indent != indent {
			break
		}
		if ln.body != "-" && !strings.HasPrefix(ln.body, "- ") {
			break
		}
		seq = append(seq, p.parseSequenceItem(indent, ln))
	}
	return seq
}

func (p *parser) parseSequenceItem(indent int, ln pline) node {
	rest := ln.body[1:] // drop the leading '-'
	k := 0
	for k < len(rest) && rest[k] == ' ' {
		k++
	}
	if k == len(rest) {
		// "-" alone: the item's value is a block on the following (deeper) lines.
		p.i++
		return p.parseNestedValue(indent)
	}
	// Rewrite the dash line to look like an ordinary line at the item's content column,
	// then parse a block starting there so subsequent keys of the item join the mapping.
	contentIndent := indent + 1 + k
	p.lines[p.i] = pline{indent: contentIndent, body: rest[k:], raw: ln.raw}
	return p.parseBlock(contentIndent)
}

// parseBlockScalar reads a literal (|) or folded (>) block scalar whose header sits at
// parentIndent, honoring an explicit indentation indicator and a chomping indicator.
func (p *parser) parseBlockScalar(parentIndent int, indicator string) string {
	style := indicator[0] // '|' or '>'
	var chomp byte        // '-', '+', or 0 (clip)
	explicit := 0
	for i := 1; i < len(indicator); i++ {
		c := indicator[i]
		switch {
		case c == '-' || c == '+':
			chomp = c
		case c >= '0' && c <= '9':
			explicit = explicit*10 + int(c-'0')
		}
	}

	var raws []string
	for p.i < len(p.lines) {
		ln := p.lines[p.i]
		if strings.TrimSpace(ln.raw) == "" {
			raws = append(raws, ln.raw)
			p.i++
			continue
		}
		if ln.indent > parentIndent {
			raws = append(raws, ln.raw)
			p.i++
			continue
		}
		break
	}

	contentIndent := parentIndent + explicit
	if explicit == 0 {
		contentIndent = parentIndent + 1
		for _, r := range raws {
			if strings.TrimSpace(r) == "" {
				continue
			}
			ci := 0
			for ci < len(r) && r[ci] == ' ' {
				ci++
			}
			contentIndent = ci
			break
		}
	}

	lines := make([]string, 0, len(raws))
	for _, r := range raws {
		if strings.TrimSpace(r) == "" {
			lines = append(lines, "")
			continue
		}
		if len(r) >= contentIndent {
			lines = append(lines, r[contentIndent:])
		} else {
			lines = append(lines, strings.TrimLeft(r, " "))
		}
	}

	var body string
	if style == '>' {
		body = fold(lines)
	} else {
		body = strings.Join(lines, "\n")
	}
	switch chomp {
	case '-':
		body = strings.TrimRight(body, "\n")
	case '+':
		// keep trailing newlines as collected
	default: // clip: a single trailing newline
		body = strings.TrimRight(body, "\n")
		if body != "" {
			body += "\n"
		}
	}
	return body
}

// fold implements the folded (>) block-scalar join: adjacent non-blank lines are joined
// with a space, blank lines become newlines.
func fold(lines []string) string {
	var b strings.Builder
	prevBlank := true
	for _, l := range lines {
		if l == "" {
			b.WriteString("\n")
			prevBlank = true
			continue
		}
		if !prevBlank {
			b.WriteString(" ")
		}
		b.WriteString(l)
		prevBlank = false
	}
	return b.String()
}

// keyColon returns the index of the ':' that separates a mapping key from its value —
// the first ':' that is followed by a space or ends the line and is not inside quotes —
// or -1 if the line is not a "key: value" form.
func keyColon(body string) int {
	inSingle, inDouble := false, false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == ':' && !inSingle && !inDouble:
			if i+1 >= len(body) || body[i+1] == ' ' {
				return i
			}
		}
	}
	return -1
}

// splitKey parses a "key: value", "key:", or "key: |" line. It returns the key, an inline
// scalar value (if present), and a block-scalar indicator ("|", ">", "|-", "|2", ...) when
// the value introduces a block scalar.
func splitKey(body string) (key, value string, hasInline bool, blockInd string) {
	sep := keyColon(body)
	if sep < 0 {
		return "", "", false, ""
	}
	key = unquote(strings.TrimSpace(body[:sep]))
	rest := strings.TrimSpace(body[sep+1:])
	if rest == "" {
		return key, "", false, ""
	}
	if ind, ok := blockIndicator(rest); ok {
		return key, "", false, ind
	}
	rest = stripInlineComment(rest)
	if rest == "" {
		return key, "", false, ""
	}
	return key, rest, true, ""
}

// blockIndicator reports whether s begins a block scalar (| or >, optionally with a
// chomping indicator and/or an explicit indentation digit), returning the indicator token.
func blockIndicator(s string) (string, bool) {
	if len(s) == 0 || (s[0] != '|' && s[0] != '>') {
		return "", false
	}
	tok := s
	if sp := strings.IndexAny(s, " \t"); sp >= 0 {
		tok = s[:sp]
		if rest := strings.TrimSpace(s[sp:]); rest != "" && !strings.HasPrefix(rest, "#") {
			return "", false // trailing content that is not a comment: not a block header
		}
	}
	for i := 1; i < len(tok); i++ {
		c := tok[i]
		if c != '+' && c != '-' && (c < '0' || c > '9') {
			return "", false
		}
	}
	return tok, true
}

func parseScalar(v string) string {
	return unquote(strings.TrimSpace(v))
}

func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	if s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		inner = strings.ReplaceAll(inner, `\"`, `"`)
		inner = strings.ReplaceAll(inner, `\n`, "\n")
		inner = strings.ReplaceAll(inner, `\t`, "\t")
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	}
	if s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}

// stripInlineComment removes a trailing " # comment" from an unquoted scalar. A '#'
// only starts a comment when it begins the string or is preceded by whitespace, and it
// is ignored inside quotes.
func stripInlineComment(s string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return strings.TrimRight(s[:i], " \t")
			}
		}
	}
	return s
}
