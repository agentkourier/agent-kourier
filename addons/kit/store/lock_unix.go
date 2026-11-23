//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile opens path and takes an exclusive, non-blocking flock on it, which the kernel releases when the file is
// closed or the process ends. Another holder makes it fail with ErrInUse, in the words of noun (Options.Noun).
func lockFile(path, noun string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the path is the operator's SQLITE_PATH plus .lock
	if err != nil {
		return nil, fmt.Errorf("open the lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, inUseError{noun}
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}
