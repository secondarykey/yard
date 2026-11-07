package config

import (
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	h, err := HashPassword("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$") {
		t.Errorf("hash = %q", h)
	}
	if h2, _ := HashPassword("s3cret"); h2 == h {
		t.Error("hashes of the same password should differ (salt)")
	}

	c, err := Parse([]byte("[admin]\npassword_hash = '"+h+"'\n"), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.AuthEnabled() {
		t.Error("auth should be on")
	}
	if !c.Admin.CheckPassword("s3cret") {
		t.Error("right password rejected")
	}
	for _, p := range []string{"", "s3cre", "s3cret ", "S3CRET"} {
		if c.Admin.CheckPassword(p) {
			t.Errorf("wrong password %q accepted", p)
		}
	}

	// Format writes it back
	d, err := Decode(Format(c))
	if err != nil || d.Admin.PasswordHash != h {
		t.Errorf("round trip = %q, %v", d.Admin.PasswordHash, err)
	}

	var off Admin
	if off.AuthEnabled() || off.CheckPassword("") {
		t.Error("no password should mean auth off")
	}
	if _, err := HashPassword(""); err == nil {
		t.Error("empty password should be rejected")
	}
}

func TestPasswordHashInvalid(t *testing.T) {
	for _, h := range []string{
		"plain",
		"pbkdf2-sha256$0$c2FsdA$a2V5",
		"pbkdf2-sha256$1000$$a2V5",
		"pbkdf2-sha256$1000$c2FsdA$!!",
		"bcrypt$1000$c2FsdA$a2V5",
	} {
		_, err := Parse([]byte("[admin]\npassword_hash = '"+h+"'\n"), t.TempDir(), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "password_hash") {
			t.Errorf("%q: err = %v", h, err)
		}
	}
}
