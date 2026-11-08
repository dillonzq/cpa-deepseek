//go:build !unix

package plugin

import "os"

// openAuthRecord opens an entry for descriptor-based type checks before reading.
// Non-Unix platforms do not require the Unix FIFO nonblocking-open flag.
func openAuthRecord(path string) (*os.File, error) {
	return os.Open(path)
}
