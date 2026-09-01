package host

import "os"

// localFS is file access on the local machine.
type localFS struct{}

func (localFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (localFS) WriteFile(path string, b []byte, perm os.FileMode) error {
	return os.WriteFile(path, b, perm)
}

func (localFS) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }

func (localFS) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }

func (localFS) RemoveAll(path string) error { return os.RemoveAll(path) }

func (localFS) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
