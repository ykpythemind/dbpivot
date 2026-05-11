package config

import (
	"fmt"
	"strings"
)

// RequiredVars extracts variable names referenced as "${VAR}" from s,
// preserving first-occurrence order and de-duplicating.
// "$$" denotes a literal "$" and is not treated as a variable reference.
func RequiredVars(s string) []string {
	var out []string
	seen := make(map[string]struct{})
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			i += 2
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return out
			}
			name := s[i+2 : i+2+end]
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				out = append(out, name)
			}
			i += 2 + end + 1
			continue
		}
		i++
	}
	return out
}

// Resolve replaces every "${VAR}" in s with vars[VAR]. Variables that are not
// present in vars are reported in missing (preserving first-occurrence order)
// and an error is returned. "$$" expands to a single "$".
// An unclosed "${" returns an error.
func Resolve(s string, vars map[string]string) (resolved string, missing []string, err error) {
	var b strings.Builder
	b.Grow(len(s))
	seenMissing := make(map[string]struct{})
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", nil, fmt.Errorf("unclosed variable reference in %q", s)
			}
			name := s[i+2 : i+2+end]
			val, ok := vars[name]
			if !ok {
				if _, dup := seenMissing[name]; !dup {
					seenMissing[name] = struct{}{}
					missing = append(missing, name)
				}
			} else {
				b.WriteString(val)
			}
			i += 2 + end + 1
			continue
		}
		return "", nil, fmt.Errorf("bare $ without ${...} in %q at index %d", s, i)
	}
	if len(missing) > 0 {
		return "", missing, fmt.Errorf("missing variables: %s", strings.Join(missing, ", "))
	}
	return b.String(), nil, nil
}
