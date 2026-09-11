//go:build !linux && !darwin

package store

import (
	"errors"
	"os"
)

func lockExclusive(_ *os.File) error {
	return errors.New("operation store requires Linux or macOS with local filesystem locking")
}
