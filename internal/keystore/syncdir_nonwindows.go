//go:build !windows

package keystore

import (
	"fmt"
	"os"
)

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync keystore directory: %w", err)
	}
	return nil
}
