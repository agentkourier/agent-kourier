//go:build !unix

package store

import (
	"errors"
	"os"
)

// lockFile is not implemented off Unix: without it two processes could share one file.
func lockFile(string, string) (*os.File, error) {
	return nil, errors.New("the SQLite store needs a Unix file lock, which this platform does not have")
}
