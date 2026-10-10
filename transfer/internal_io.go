package transfer

import "os"

// readFile is a tiny indirection so tests can stub the
// token-file read without monkey-patching os.ReadFile.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // path is operator-controlled (sidecar mount)
}
