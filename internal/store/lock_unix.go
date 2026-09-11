//go:build linux || darwin

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockExclusive(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrLocked
		}
		return fmt.Errorf("lock operation store: %w", err)
	}
	return nil
}
