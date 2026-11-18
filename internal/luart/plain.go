package luart

import (
	"errors"
	"os"
	"syscall"
)

var (
	errIsDir      = errors.New("is a directory")
	errNotRegular = errors.New("not a regular file: named pipes, devices and sockets are refused")
)

// checkPlain accepts a regular file, and a directory when dirOK.
func checkPlain(info os.FileInfo, dirOK bool) error {
	switch {
	case info.IsDir():
		if dirOK {
			return nil
		}
		return errIsDir
	case info.Mode().IsRegular():
		return nil
	}
	return errNotRegular
}

// openPlain opens abs for a host file operation without ever waiting on a special
// file: opening or reading a named pipe or device waits for a peer that may never
// come. A stat refuses known special files; O_NONBLOCK makes the open itself return
// at once; the type is checked again on the open descriptor, so a path replaced
// between the two checks is refused too.
func openPlain(abs string, flag int, perm os.FileMode, dirOK bool) (*os.File, os.FileInfo, error) {
	if info, err := os.Stat(abs); err == nil {
		if err := checkPlain(info, dirOK); err != nil {
			return nil, nil, err
		}
	}
	f, err := os.OpenFile(abs, flag|syscall.O_NONBLOCK, perm)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = checkPlain(info, dirOK)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}
