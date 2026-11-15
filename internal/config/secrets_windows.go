//go:build windows

package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// SIDs bien conocidos: SYSTEM y el grupo Administradores. Se usan SIDs y no
// nombres para que funcione en Windows en español o inglés.
const (
	sidLocalSystem    = "*S-1-5-18:F"
	sidAdministrators = "*S-1-5-32-544:F"
)

func protectPath(path string, dir bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if dir {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return fmt.Errorf("crear %s: %w", abs, err)
		}
	}

	grants := []string{sidLocalSystem, sidAdministrators}
	// El usuario que ejecuta conserva acceso para el modo consola.
	if sid, err := currentUserSID(); err == nil && sid != "" {
		grants = append(grants, "*"+sid+":F")
	}

	args := append([]string{abs, "/inheritance:r", "/grant:r"}, grants...)
	out, err := exec.Command("icacls", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls %s: %w (%s)", abs, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func currentUserSID() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer func() { _ = token.Close() }()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	if user == nil || user.User.Sid == nil {
		return "", nil
	}
	return user.User.Sid.String(), nil
}
