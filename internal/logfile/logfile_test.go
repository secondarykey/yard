package logfile

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDaily(t *testing.T) {
	dir := t.TempDir()
	w, err := Daily(filepath.Join(dir, "sub", "access.log"), Options{})
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
	if _, err := Daily(filepath.Join(file, "yard.log"), Options{Markers: true}); err == nil {
		t.Error("expected an error for a directory that is a file")
	}
}

// Old files of the same name are compressed / deleted when the log is opened.
func TestDailyCleanup(t *testing.T) {
	dir := t.TempDir()
	day := func(n int) string { return time.Now().AddDate(0, 0, -n).Format("20060102") }
	for _, n := range []int{1, 3, 10} {
		os.WriteFile(filepath.Join(dir, "yard_"+day(n)+".log"), []byte("x\n"), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "other_"+day(10)+".log"), nil, 0o644)

	w, err := Daily(filepath.Join(dir, "yard.log"), Options{CompressAfter: 2, Keep: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	want := map[string]bool{
		"yard_" + day(1) + ".log":    true,
		"yard_" + day(3) + ".log.gz": true,
		"other_" + day(10) + ".log":  true,
		"yard_" + day(0) + ".log":    true,
	}
	// The cleanup runs in the background.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, _ := os.ReadDir(dir)
		got := map[string]bool{}
		for _, e := range entries {
			got[e.Name()] = true
		}
		if reflect.DeepEqual(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("files = %v, want %v", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
