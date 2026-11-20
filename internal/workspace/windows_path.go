package workspace

import "strings"

// windowsMetadataPath accepts the absolute paths produced by Identify. Keep
// both Windows namespace prefixes intact; only ordinary UNC paths need UNC
// conversion. This helper is portable so its path contracts run on every host.
func windowsMetadataPath(path string) string {
	switch {
	case strings.HasPrefix(path, `\\?\`), strings.HasPrefix(path, `\\.\`):
		return path
	case strings.HasPrefix(path, `\\`):
		return `\\?\UNC\` + path[2:]
	default:
		return `\\?\` + path
	}
}
