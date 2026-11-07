// Package logfile opens yard's own logs (yard.log and access_log) as one file per day.
//
// Server output (`[[server]] log`) does not use this: it goes to the configured file as is,
// since it is passed to the server process as an *os.File.
package logfile

import (
	"path/filepath"
	"strings"

	wlog "github.com/wenteasy/log"
)

// Daily returns a writer that switches files every day.
// The date goes before the extension, and the extension becomes .log:
// "dir/access.log" is written to "dir/access_20261108.log". Old files are kept.
//
// The first file is opened here, so that an unwritable location is an error now rather than
// a lost line later. With markers off, nothing is written to the file until the first line.
func Daily(name string, markers bool) (*wlog.RollingFileWriter, error) {
	w, err := wlog.NewRollingFileWriter(filepath.Dir(name), wlog.Day)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(name)
	w.SetPrefix(strings.TrimSuffix(base, filepath.Ext(base)))
	if !markers {
		w.DisableMarkers()
	}
	if _, err := w.Write(nil); err != nil {
		return nil, err
	}
	return w, nil
}
