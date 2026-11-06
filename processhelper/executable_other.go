//go:build !linux

package processhelper

import "os"

func helperExecutable() (string, error) {
	return os.Executable()
}
