package config

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// Keys that SetServerFlag can change.
const (
	KeyDisabled = "disabled"
	KeyLog      = "log"
)

var (
	serverHeader = regexp.MustCompile(`^\s*\[\[\s*server\s*\]\]`)
	tableHeader  = regexp.MustCompile(`^\s*\[`)
)

// SetServerFlag sets a boolean key (KeyDisabled or KeyLog) of the server named name in the
// configuration text data, and returns the new text with LF line endings.
//
// Only that one line is changed (or added), so comments and the rest of the file are kept as written.
// If the text is laid out in a way that this line edit cannot handle, the result would not decode to
// the intended configuration; the whole file is then written out with Format instead (without comments).
func SetServerFlag(data []byte, name, key string, v bool) ([]byte, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	c, err := Decode([]byte(text))
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, s := range c.Servers {
		if s.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("unknown server: %s", name)
	}
	want, err := Decode([]byte(text))
	if err != nil {
		return nil, err
	}
	switch key {
	case KeyDisabled:
		want.Servers[idx].Disabled = v
	case KeyLog:
		want.Servers[idx].Log = v
	default:
		return nil, fmt.Errorf("unknown key: %s", key)
	}
	if reflect.DeepEqual(c, want) {
		return []byte(text), nil
	}

	if out, ok := setLine(text, idx, key, v); ok {
		if got, err := Decode([]byte(out)); err == nil && reflect.DeepEqual(got, want) {
			return []byte(out), nil
		}
	}
	return Format(want), nil
}

// setLine sets "key = v" in the idx-th [[server]] table: it rewrites the key's line if there is one,
// and otherwise adds a line after the table's last key. It reports false if there is no such table.
func setLine(text string, idx int, key string, v bool) (string, bool) {
	lines := strings.Split(text, "\n")
	start, n := -1, -1
	for i, l := range lines {
		if serverHeader.MatchString(l) {
			if n++; n == idx {
				start = i
				break
			}
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if tableHeader.MatchString(lines[i]) {
			end = i
			break
		}
	}

	value := fmt.Sprint(v)
	keyLine := regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(key) + `\s*=\s*)(true|false)(\s*(#.*)?)$`)
	last := start
	for i := start + 1; i < end; i++ {
		if m := keyLine.FindStringSubmatch(lines[i]); m != nil {
			lines[i] = m[1] + value + m[3]
			return strings.Join(lines, "\n"), true
		}
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			last = i
		}
	}
	lines = append(lines[:last+1], append([]string{key + " = " + value}, lines[last+1:]...)...)
	return strings.Join(lines, "\n"), true
}
