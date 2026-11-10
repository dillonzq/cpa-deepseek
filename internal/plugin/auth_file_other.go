//go:build !unix

package plugin

import "os"

// openAuthRecord opens an entry for descriptor-based type checks before reading.
// Non-Unix platforms do not require the Unix FIFO nonblocking-open flag.
func openAuthRecord(path string) (*os.File, error) {
	return os.Open(path)
}

func openCleanupRecord(root *os.Root, name string, flag int) (*os.File, error) {
	return root.OpenFile(name, flag, 0)
}
