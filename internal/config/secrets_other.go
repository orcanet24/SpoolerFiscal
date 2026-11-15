//go:build !windows

package config

import "os"

func protectPath(path string, dir bool) error {
	if dir {
		return os.MkdirAll(path, 0o755)
	}
	return nil
}
