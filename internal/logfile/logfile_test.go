package logfile

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDaily(t *testing.T) {
	dir := t.TempDir()
	w, err := Daily(filepath.Join(dir, "sub", "access.log"), false)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "line\n")
	w.Close()

	name := filepath.Join(dir, "sub", "access_"+time.Now().Format("20060102")+".log")
	if b, err := os.ReadFile(name); err != nil || string(b) != "line\n" {
		t.Errorf("%s = %q, %v", name, b, err)
	}
}

func TestDailyUnwritable(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o644)
	if _, err := Daily(filepath.Join(file, "yard.log"), true); err == nil {
		t.Error("expected an error for a directory that is a file")
	}
}
