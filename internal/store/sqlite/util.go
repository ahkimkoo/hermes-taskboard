package sqlite

import "os"

func ensureDir(p string) error {
	if p == "" {
		return nil
	}
	return os.MkdirAll(p, 0o700)
}
