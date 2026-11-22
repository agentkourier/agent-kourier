//go:build !unix

package store

import (
	"errors"
	"os"
)

// lockFile is not implemented off Unix: without it two agents could share one file.
func lockFile(string) (*os.File, error) {
	return nil, errors.New("the SQLite store needs a Unix file lock, which this platform does not have")
}
