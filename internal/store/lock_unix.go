//go:build unix

package store

import (
	"fmt"
	"os"
	"syscall"
)

// acquireLock takes an exclusive advisory flock on a dedicated lock file so
// two jobwatch processes can't race on the same state (both would notify
// the same jobs and clobber each other's saves). The lock file is separate
// from the state file because Save's rename replaces the state file's
// inode, which would defeat a lock held on it.
func acquireLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another jobwatch instance is already running (lock %s held)", path)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
