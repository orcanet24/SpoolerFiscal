// Package logging configura slog con salida a consola y archivo rotado.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"

	"spoolerfiscal/internal/config"
)

// Setup construye el logger del servicio. Devuelve también la función de
// cierre que vuelca y libera el archivo de log.
func Setup(cfg config.Logging, defaultFile string) (*slog.Logger, func() error, error) {
	level := parseLevel(cfg.Level)
	file := strings.TrimSpace(cfg.File)
	if file == "" {
		file = defaultFile
	}

	sinks := []io.Writer{}
	var closer func() error = func() error { return nil }

	if cfg.Console {
		sinks = append(sinks, os.Stderr)
	}
	if file != "" {
		if err := config.EnsureDir(file); err != nil {
			return nil, closer, fmt.Errorf("crear directorio de logs: %w", err)
		}
		rot := &lumberjack.Logger{
			Filename:   file,
			MaxSize:    positive(cfg.MaxSizeMB, 20),
			MaxBackups: positive(cfg.MaxBackups, 10),
			MaxAge:     positive(cfg.MaxAgeDays, 30),
			Compress:   cfg.Compress,
			LocalTime:  true,
		}
		sinks = append(sinks, rot)
		closer = rot.Close
	}
	if len(sinks) == 0 {
		sinks = append(sinks, io.Discard)
	}

	handler := slog.NewTextHandler(io.MultiWriter(sinks...), &slog.HandlerOptions{Level: level})
	return slog.New(handler), closer, nil
}

func positive(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
