package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LoadDotEnv loads KEY=VALUE entries without replacing variables already set
// in the process environment. A missing file is allowed.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("abrir archivo de entorno %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("%s:%d: se esperaba KEY=VALUE", path, lineNo)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'')) {
			if value[0] == '"' {
				value, err = strconv.Unquote(value)
				if err != nil {
					return fmt.Errorf("%s:%d: valor entre comillas inválido: %w", path, lineNo, err)
				}
			} else {
				value = value[1 : len(value)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("%s:%d: establecer %s: %w", path, lineNo, key, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("leer archivo de entorno %s: %w", path, err)
	}
	return nil
}

// LoadExecutableDotEnv loads the .env file located beside the running binary.
func LoadExecutableDotEnv() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("ubicar ejecutable para cargar .env: %w", err)
	}
	return LoadDotEnv(filepath.Join(filepath.Dir(exe), ".env"))
}
