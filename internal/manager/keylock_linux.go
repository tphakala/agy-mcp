package manager

import (
	"errors"
	"syscall"
)

// flockExclusiveNB takes a non-blocking exclusive flock on fd. It returns
// (true, nil) on success, (false, nil) when another open file description already
// holds the lock (EWOULDBLOCK), and (false, err) on any other error. The flock is
// associated with the open file description, so it is released when fd is closed.
func flockExclusiveNB(fd uintptr) (bool, error) {
	err := syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}
