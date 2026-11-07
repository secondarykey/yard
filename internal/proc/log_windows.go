package proc

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// openLog creates (or empties) a server's output file, creating the directory if needed.
//
// os.OpenFile does not share delete access on Windows, so a file it opened cannot be renamed while
// the server holds it. The file is opened here with FILE_SHARE_DELETE so that nameLog can rename it
// after the server has started. It is append-only, like O_APPEND.
func openLog(name string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	h, err := windows.CreateFile(p,
		windows.FILE_APPEND_DATA|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(h), name), nil
}
