package localnet

import (
	"crypto/rand"
	"errors"
	"os"
	"strings"
)

// EnsureToken creates a private bearer credential for this local runtime.
func EnsureToken(file string) (string, error) {
	if data, err := os.ReadFile(file); err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) < 26 || len(token) > 256 {
			return "", errors.New("invalid runtime token file")
		}
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	token := rand.Text()
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, writeErr := f.WriteString(token + "\n")
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return "", err
	}
	return token, nil
}
