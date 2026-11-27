package remote

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// The formats caddy-dns/cloudflare v0.2.4 accepts. Its own check fails with an error that quotes
// the token, so devingress checks first and names only the file.
var (
	newToken    = regexp.MustCompile(`^cf(ut|at)_[A-Za-z0-9_-]{32,256}$`)
	legacyToken = regexp.MustCompile(`^[A-Za-z0-9_-]{35,50}$`)
)

// ReadToken reads the Cloudflare API token in path as Caddy's {file.*} placeholder does, one
// trailing "\n" and then one "\r" removed, and checks its format. Errors name path, never what
// it holds.
func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("token file %s does not exist: save the Cloudflare API token there, mode 600", path)
	case err != nil:
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	switch {
	case strings.TrimSpace(token) == "":
		return "", fmt.Errorf("token file %s is empty: save the Cloudflare API token there, mode 600", path)
	case !newToken.MatchString(token) && !legacyToken.MatchString(token):
		return "", fmt.Errorf("token file %s does not hold a Cloudflare API token "+
			"(cfut_ or cfat_ and 32-256 letters, digits, '_' or '-'; or a 35-50 character legacy token): "+
			"it must hold only the token, without quotes, spaces or extra lines", path)
	}
	return token, nil
}

// TokenModeWarning is a warning when the token file at path can be read by its group or by
// others, "" otherwise.
func TokenModeWarning(path string) string {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("token file %s is readable by its group or others (mode %04o): chmod 600 %s",
		path, fi.Mode().Perm(), path)
}

// Redact replaces each occurrence of token in s.
func Redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[redacted token]")
}
