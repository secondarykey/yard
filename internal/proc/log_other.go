//go:build !windows

package proc

import (
	"os"
	"path/filepath"
)

// openLog creates (or empties) a server's output file, creating the directory if needed.
// It can be renamed while open (nameLog).
func openLog(name string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o644)
}
