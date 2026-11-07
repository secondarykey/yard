package config

import (
	"strings"
	"testing"
)

const editSrc = `# yard
[yard]
log = true

# first
[[server]]
name = "a"   # keep me
path = "/a"
command = "a"
disabled = false # off for now

[[server]]
name = "b"
path = "/b"
command = "b"
args = [
  "-x",
]

# trailing comment
`

func TestSetServerFlag(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		v         bool
		want      string // the edited part
	}{
		{"a", KeyDisabled, true, "disabled = true # off for now\n"},
		{"a", KeyLog, true, "disabled = false # off for now\nlog = true\n\n[[server]]"},
		{"b", KeyLog, true, "]\nlog = true\n\n# trailing comment"},
		{"b", KeyDisabled, true, "]\ndisabled = true\n"},
	} {
		out, err := SetServerFlag([]byte(strings.ReplaceAll(editSrc, "\n", "\r\n")), tc.name, tc.key, tc.v)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.name, tc.key, err)
		}
		s := string(out)
		if !strings.Contains(s, tc.want) || !strings.Contains(s, "# keep me") || strings.Contains(s, "\r") {
			t.Errorf("%s %s = %v:\n%s", tc.name, tc.key, tc.v, s)
		}
		c, err := Decode(out)
		if err != nil {
			t.Fatal(err)
		}
		for _, sv := range c.Servers {
			got := map[string]bool{KeyDisabled: sv.Disabled, KeyLog: sv.Log}[tc.key]
			if want := sv.Name == tc.name && tc.v; got != want {
				t.Errorf("%s %s: server %s has %v", tc.name, tc.key, sv.Name, got)
			}
		}
	}
}

func TestSetServerFlagUnchanged(t *testing.T) {
	out, err := SetServerFlag([]byte(editSrc), "b", KeyLog, false)
	if err != nil || string(out) != editSrc {
		t.Errorf("err = %v, out =\n%s", err, out)
	}
	if _, err := SetServerFlag([]byte(editSrc), "zzz", KeyLog, true); err == nil {
		t.Error("unknown server should be an error")
	}
}

// A layout the line edit gets wrong falls back to Format.
func TestSetServerFlagFallback(t *testing.T) {
	src := "server = [{name = \"a\", path = \"/a\", command = \"a\"}]\n"
	out, err := SetServerFlag([]byte(src), "a", KeyLog, true)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := Decode(out); err != nil || !c.Servers[0].Log {
		t.Errorf("err = %v, out =\n%s", err, out)
	}
	if _, err := SetServerFlag([]byte("[[server]]\nname = \"a\"\nlog = 'yes'\n"), "a", KeyLog, true); err == nil {
		t.Error("a file that does not decode should be an error")
	}
}
