// Package logfile opens yard's own logs (yard.log and access_log) as one file per day.
//
// Server output (`[[server]] log`) does not use this: it goes to the configured file as is,
// since it is passed to the server process as an *os.File.
package logfile

import (
	"log/slog"
	"path/filepath"
	"strings"

	wlog "github.com/wenteasy/log"
)

// Options are the settings of one daily log.
type Options struct {
	Markers       bool // write a separator line when reopening an existing file and when closing
	CompressAfter int  // gzip files older than this many days; 0 = never
	Keep          int  // delete files older than this many days; 0 = never
}

// Daily returns a writer that switches files every day.
// The date goes before the extension, and the extension becomes .log:
// "dir/access.log" is written to "dir/access_20261108.log". Old files are compressed /
// deleted only as Options says; that runs in the background whenever a file is opened.
//
// The first file is opened here, so that an unwritable location is an error now rather than
// a lost line later. With markers off, nothing is written to the file until the first line.
func Daily(name string, opts Options) (*wlog.RollingFileWriter, error) {
	w, err := wlog.NewRollingFileWriter(filepath.Dir(name), wlog.Day)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(name)
	w.SetPrefix(strings.TrimSuffix(base, filepath.Ext(base)))
	if !opts.Markers {
		w.DisableMarkers()
	}
	w.SetCompressAfter(opts.CompressAfter)
	w.SetKeep(opts.Keep)
	// slog.Default() is read when the error happens, so this works before main has set it up,
	// and writing back into w from here is allowed by RollingFileWriter.
	w.SetErrorHandler(func(err error) { slog.Warn("log files", "err", err) })
	if _, err := w.Write(nil); err != nil {
		return nil, err
	}
	return w, nil
}
