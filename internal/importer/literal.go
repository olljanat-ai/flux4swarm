package importer

import (
	"reflect"
	"strconv"
	"strings"
	"unicode"

	"sigs.k8s.io/yaml"
)

// literalPlaceholder marks a string value that marshalLiteral renders as a
// literal block scalar itself.
const literalPlaceholder = "__flux4swarm_literal_"

// marshalLiteral marshals m like yaml.Marshal, but writes multi-line strings
// that the YAML library would emit double-quoted and line-folded as literal
// block scalars (|). The library refuses block style for content with
// trailing spaces or tabs, which config files (Prometheus rules, scripts,
// templates) often have; YAML itself allows them in literal blocks.
//
// The value must stay byte-for-byte the same: strings with characters a
// literal block cannot hold are left to the library, and the result is
// parsed back and compared, falling back to plain yaml.Marshal on any
// difference.
func marshalLiteral(m map[string]any) ([]byte, error) {
	plain, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(plain), literalPlaceholder) {
		return plain, nil
	}
	var lits []string
	swapped := swapLiterals(m, &lits)
	if len(lits) == 0 {
		return plain, nil
	}
	b, err := yaml.Marshal(swapped)
	if err != nil {
		return plain, nil
	}
	out := expandLiterals(string(b), lits)
	if out == "" {
		return plain, nil
	}
	var want, got any
	if yaml.Unmarshal(plain, &want) != nil || yaml.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(want, got) {
		return plain, nil
	}
	return []byte(out), nil
}

// swapLiterals returns a copy of v with every string that needs our
// literal rendering replaced by a placeholder, collecting the originals.
func swapLiterals(v any, lits *[]string) any {
	switch t := v.(type) {
	case map[string]any:
		c := make(map[string]any, len(t))
		for k, val := range t {
			c[k] = swapLiterals(val, lits)
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, val := range t {
			c[i] = swapLiterals(val, lits)
		}
		return c
	case string:
		if needsLiteral(t) {
			*lits = append(*lits, t)
			return literalPlaceholder + strconv.Itoa(len(*lits)-1) + "__"
		}
	}
	return v
}

// needsLiteral reports whether s is multi-line text that a literal block
// can hold exactly but the YAML library would not write as one.
func needsLiteral(s string) bool {
	if !strings.Contains(s, "\n") {
		return false
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && !unicode.IsPrint(r) {
			return false // CR, other control chars, BOM, ...
		}
	}
	// The first non-empty line decides the block's indentation; leading
	// whitespace there would need an explicit indentation indicator.
	for _, l := range strings.Split(s, "\n") {
		if l == "" {
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			return false
		}
		break
	}
	if strings.TrimRight(s, "\n") == "" {
		return false
	}
	// Leave strings the library already writes as literal blocks alone so
	// existing output does not change.
	b, err := yaml.Marshal(s)
	return err == nil && !strings.HasPrefix(string(b), "|")
}

// expandLiterals replaces each placeholder line with a literal block. It
// returns "" when a placeholder is not where it is expected.
func expandLiterals(doc string, lits []string) string {
	lines := strings.SplitAfter(doc, "\n")
	var sb strings.Builder
	found := 0
	for _, line := range lines {
		body := strings.TrimSuffix(line, "\n")
		i := strings.LastIndex(body, literalPlaceholder)
		if i < 0 {
			sb.WriteString(line)
			continue
		}
		head, tok := body[:i], body[i+len(literalPlaceholder):]
		n, err := strconv.Atoi(strings.TrimSuffix(tok, "__"))
		if err != nil || !strings.HasSuffix(tok, "__") || n < 0 || n >= len(lits) {
			return ""
		}
		if !strings.HasSuffix(head, ": ") && !strings.HasSuffix(head, "- ") {
			return ""
		}
		found++
		// Content goes two columns past the start of the entry (after any
		// "- " sequence markers).
		col := len(head) - len(strings.TrimLeft(head, " "))
		for strings.HasPrefix(head[col:], "- ") {
			col += 2
		}
		indent := strings.Repeat(" ", col+2)
		s := lits[n]
		chomp := "-"
		switch {
		case strings.HasSuffix(s, "\n\n"):
			chomp = "+"
		case strings.HasSuffix(s, "\n"):
			chomp = ""
		}
		sb.WriteString(head + "|" + chomp + "\n")
		content := s
		if chomp != "-" {
			content = strings.TrimSuffix(s, "\n")
		}
		for _, l := range strings.Split(content, "\n") {
			if l == "" {
				sb.WriteString("\n")
				continue
			}
			sb.WriteString(indent + l + "\n")
		}
	}
	if found != len(lits) {
		return ""
	}
	return sb.String()
}
