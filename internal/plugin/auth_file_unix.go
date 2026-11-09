//go:build unix

package plugin

import (
	"os"
	"syscall"
)

// openAuthRecord avoids blocking on a FIFO, even if a regular file is replaced
// before open. The caller checks the opened descriptor's type before reading.
func openAuthRecord(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func openCleanupRecord(root *os.Root, name string, flag int) (*os.File, error) {
	return root.OpenFile(name, flag|syscall.O_NONBLOCK, 0)
}
