// Package yamlite parses the block-style YAML subset used by model.yaml
// without adding a general YAML dependency. Unsupported syntax returns an error.
// Values decode to map[string]any, []any, string, bool, int64, float64 and nil.
package yamlite

import (
	"fmt"
	"strconv"
	"strings"
)

type line struct {
	num    int // 1-based, for errors
	indent int
	text   string // content with indentation and trailing comment removed
	raw    string // the full line, for block scalars
}

type parser struct {
	lines []line
	pos   int
}

func Parse(data []byte) (any, error) {
	p := &parser{}
	for i, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if strings.ContainsRune(raw, '\t') && strings.TrimLeft(raw, " ") != strings.TrimLeft(raw, " \t") {
			return nil, fmt.Errorf("line %d: tab indentation", i+1)
		}
		text := strings.TrimRight(stripComment(raw), " ")
		trimmed := strings.TrimLeft(text, " ")
		if trimmed == "---" && len(p.lines) == 0 {
			continue // a leading document marker is harmless
		}
		if trimmed == "---" || trimmed == "..." {
			return nil, fmt.Errorf("line %d: multiple documents are not supported", i+1)
		}
		p.lines = append(p.lines, line{num: i + 1, indent: len(text) - len(trimmed), text: trimmed, raw: raw})
	}
	p.skipBlank()
	if p.pos >= len(p.lines) {
		return nil, nil
	}
	v, err := p.node(p.lines[p.pos].indent)
	if err != nil {
		return nil, err
	}
	p.skipBlank()
	if p.pos < len(p.lines) {
		l := p.lines[p.pos]
		return nil, fmt.Errorf("line %d: unexpected indentation", l.num)
	}
	return v, nil
}

func (p *parser) skipBlank() {
	for p.pos < len(p.lines) && p.lines[p.pos].text == "" {
		p.pos++
	}
}

func (p *parser) peek() (line, bool) {
	p.skipBlank()
	if p.pos >= len(p.lines) {
		return line{}, false
	}
	return p.lines[p.pos], true
}

func isSeqItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

// node parses whatever starts at the current line, which is at indent ind.
func (p *parser) node(ind int) (any, error) {
	l, ok := p.peek()
	if !ok {
		return nil, nil
	}
	if isSeqItem(l.text) {
		return p.sequence(ind)
	}
	if _, _, isKey, err := splitKey(l.text); err != nil {
		return nil, fmt.Errorf("line %d: %w", l.num, err)
	} else if isKey {
		return p.mapping(ind)
	}
	p.pos++
	return scalar(l.text, l.num)
}

func (p *parser) mapping(ind int) (map[string]any, error) {
	out := map[string]any{}
	for {
		l, ok := p.peek()
		if !ok || l.indent < ind {
			return out, nil
		}
		if l.indent > ind {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.num)
		}
		if isSeqItem(l.text) {
			return out, nil // the enclosing sequence continues
		}
		key, rest, isKey, err := splitKey(l.text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", l.num, err)
		}
		if !isKey {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", l.num)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", l.num, key)
		}
		p.pos++
		v, err := p.value(ind, rest, l.num)
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
}

// value parses what follows "key:" on a line at indent ind.
func (p *parser) value(ind int, rest string, num int) (any, error) {
	if rest == "" {
		next, ok := p.peek()
		switch {
		case !ok:
			return nil, nil
		case next.indent > ind:
			return p.node(next.indent)
		case next.indent == ind && isSeqItem(next.text):
			// YAML lets a sequence sit at its key's own indentation.
			return p.sequence(ind)
		default:
			return nil, nil
		}
	}
	if rest[0] == '|' || rest[0] == '>' {
		return p.blockScalar(ind, rest, num)
	}
	return scalar(rest, num)
}

func (p *parser) sequence(ind int) ([]any, error) {
	out := []any{}
	for {
		l, ok := p.peek()
		if !ok || l.indent < ind || (l.indent == ind && !isSeqItem(l.text)) {
			return out, nil
		}
		if l.indent > ind {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.num)
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		if rest == "" {
			p.pos++
			next, ok := p.peek()
			if !ok || next.indent <= ind {
				out = append(out, nil)
				continue
			}
			v, err := p.node(next.indent)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			continue
		}
		// "- key: value" opens a mapping whose other keys line up with "key".
		// Rewrite the line as though the dash were indentation and parse it
		// as a node at that column.
		col := l.indent + (len(l.text) - len(rest))
		p.lines[p.pos] = line{num: l.num, indent: col, text: rest, raw: l.raw}
		v, err := p.node(col)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

// blockScalar reads a literal (|) or folded (>) scalar.
func (p *parser) blockScalar(ind int, header string, num int) (any, error) {
	style, chomp := header[0], byte(0)
	for _, c := range header[1:] {
		switch c {
		case '-', '+':
			chomp = byte(c)
		default:
			return nil, fmt.Errorf("line %d: unsupported block scalar header %q", num, header)
		}
	}
	var body []string
	blockInd := -1
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		rawTrim := strings.TrimLeft(l.raw, " ")
		if rawTrim == "" {
			body = append(body, "")
			p.pos++
			continue
		}
		rawInd := len(l.raw) - len(rawTrim)
		if blockInd < 0 {
			if rawInd <= ind {
				break
			}
			blockInd = rawInd
		}
		if rawInd < blockInd {
			break
		}
		// Comments do not exist inside block scalars: use the raw line.
		body = append(body, strings.TrimRight(l.raw[blockInd:], " "))
		p.pos++
	}
	// Trailing blank lines belong to chomping, not content.
	trailing := 0
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
		trailing++
	}
	var s string
	if style == '|' {
		s = strings.Join(body, "\n")
	} else {
		var b strings.Builder
		for i, ln := range body {
			switch {
			case ln == "":
				b.WriteByte('\n')
			case i > 0 && body[i-1] != "":
				b.WriteByte(' ')
			}
			b.WriteString(ln)
		}
		s = b.String()
	}
	switch chomp {
	case '-':
	case '+':
		s += "\n" + strings.Repeat("\n", trailing)
	default:
		if len(body) > 0 {
			s += "\n"
		}
	}
	return s, nil
}

// splitKey splits "key: value". isKey is false when the text is not a mapping
// entry at all.
func splitKey(text string) (key, rest string, isKey bool, err error) {
	if text == "" {
		return "", "", false, nil
	}
	switch text[0] {
	case '"', '\'':
		end := closingQuote(text)
		if end < 0 {
			return "", "", false, nil
		}
		after := text[end+1:]
		if !(after == ":" || strings.HasPrefix(after, ": ")) {
			return "", "", false, nil
		}
		k, err := unquote(text[:end+1])
		if err != nil {
			return "", "", false, err
		}
		return k, strings.TrimSpace(after[1:]), true, nil
	case '[', '{', '&', '*', '!', '?':
		if text[0] == '?' && (len(text) == 1 || text[1] == ' ') {
			return "", "", false, fmt.Errorf("complex keys are not supported")
		}
		return "", "", false, nil
	}
	i := strings.Index(text, ": ")
	if i < 0 {
		if strings.HasSuffix(text, ":") {
			return text[:len(text)-1], "", true, nil
		}
		return "", "", false, nil
	}
	return text[:i], strings.TrimSpace(text[i+2:]), true, nil
}

func closingQuote(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case q == '\'' && s[i] == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case s[i] == q:
			return i
		}
	}
	return -1
}

func unquote(s string) (string, error) {
	if s[0] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	// YAML double-quoted escapes are a superset of Go's for the common
	// cases; anything Go cannot decode is rejected rather than misread.
	v, err := strconv.Unquote(s)
	if err != nil {
		return "", fmt.Errorf("unsupported escape in %s", s)
	}
	return v, nil
}

func scalar(s string, num int) (any, error) {
	switch s[0] {
	case '"', '\'':
		end := closingQuote(s)
		if end != len(s)-1 {
			return nil, fmt.Errorf("line %d: unterminated or trailing text after quoted string", num)
		}
		v, err := unquote(s)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", num, err)
		}
		return v, nil
	case '&', '*', '!':
		return nil, fmt.Errorf("line %d: anchors, aliases and tags are not supported", num)
	case '[', '{':
		switch s {
		case "[]":
			return []any{}, nil
		case "{}":
			return map[string]any{}, nil
		}
		if s[0] == '[' && strings.HasSuffix(s, "]") {
			return flowSeq(s[1:len(s)-1], num)
		}
		return nil, fmt.Errorf("line %d: flow mappings are not supported", num)
	case '|', '>':
		return nil, fmt.Errorf("line %d: block scalar in an unsupported position", num)
	}
	switch s {
	case "~", "null", "Null", "NULL":
		return nil, nil
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !strings.ContainsAny(s, "xXpP_") {
		return f, nil
	}
	return s, nil
}

// flowSeq handles a flat [a, b, "c"] of scalars; enough for short lists.
func flowSeq(inner string, num int) ([]any, error) {
	out := []any{}
	for _, part := range splitFlow(inner) {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("line %d: empty flow sequence entry", num)
		}
		if part[0] == '[' || part[0] == '{' {
			return nil, fmt.Errorf("line %d: nested flow collections are not supported", num)
		}
		v, err := scalar(part, num)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func splitFlow(s string) []string {
	var parts []string
	start, quote := 0, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ',':
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	if strings.TrimSpace(s[start:]) != "" || len(parts) > 0 {
		parts = append(parts, s[start:])
	}
	return parts
}

// stripComment removes a trailing comment: '#' at the start or after a space,
// outside quotes.
func stripComment(s string) string {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			// Only a quote that opens a scalar starts quoting; an apostrophe
			// inside plain text ("it's") does not.
			if i == 0 || s[i-1] == ' ' || s[i-1] == '[' || s[i-1] == ',' {
				quote = c
			}
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i]
		}
	}
	return s
}
