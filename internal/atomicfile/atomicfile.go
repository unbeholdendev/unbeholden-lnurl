// Package atomicfile replaces files so that a crash leaves either the old
// content or the new, never a mix.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces path with data, readable only by its owner, creating the
// directory if needed. The data is synced to disk before the rename. An error
// means path still holds its old content.
func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	// The file is replaced by now, so an error here would wrongly tell the
	// caller the old content is still in place.
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
