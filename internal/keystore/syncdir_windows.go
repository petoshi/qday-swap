//go:build windows

package keystore

// Windows does not support flushing a directory handle with os.File.Sync.
// The keystore file itself is flushed before it is linked into place.
func syncDirectory(string) error {
	return nil
}
