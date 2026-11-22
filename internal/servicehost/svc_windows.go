//go:build windows

package servicehost

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
	"golang.org/x/sys/windows/svc/mgr"
)

func isWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// handler implementa svc.Handler: arranca la aplicación y atiende Stop/Shutdown.
type handler struct {
	app        App
	configPath string
}

func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.app.Run(ctx, h.configPath) }()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			if err != nil {
				slog.Error("el servicio terminó con error", "servicio", h.app.Name, "error", err)
				return false, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				slog.Info("deteniendo servicio", "servicio", h.app.Name)
				cancel()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					slog.Warn("cierre forzado del servicio", "servicio", h.app.Name)
				}
				return false, 0
			}
		}
	}
}

func runService(app App, configPath string) error {
	prepareServiceCWD()
	return svc.Run(app.Name, &handler{app: app, configPath: configPath})
}

func runDebug(app App, configPath string) error {
	prepareServiceCWD()
	return debug.Run(app.Name, &handler{app: app, configPath: configPath})
}

// prepareServiceCWD fija el directorio de trabajo en el del ejecutable. El
// Administrador de servicios de Windows arranca los procesos con CWD en
// System32: sin esto, las rutas relativas de la configuración (data\spooler.db,
// logs\server.log) acabarían dentro de C:\Windows\System32.
func prepareServiceCWD() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	_ = os.Chdir(filepath.Dir(exe))
}

func installService(app App, configPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("ruta del ejecutable: %w", err)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("conectar con el administrador de servicios (ejecute como administrador): %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	if s, err := m.OpenService(app.Name); err == nil {
		_ = s.Close()
		return fmt.Errorf("el servicio %q ya existe; desinstálelo primero con -uninstall", app.Name)
	}

	s, err := m.CreateService(app.Name, exe, mgr.Config{
		DisplayName: app.DisplayName,
		Description: app.Description,
		StartType:   mgr.StartAutomatic,
	}, "-config", configPath)
	if err != nil {
		return fmt.Errorf("crear servicio: %w", err)
	}
	return s.Close()
}

func uninstallService(app App) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("conectar con el administrador de servicios (ejecute como administrador): %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	s, err := m.OpenService(app.Name)
	if err != nil {
		return fmt.Errorf("el servicio %q no está instalado: %w", app.Name, err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Control(svc.Stop); err == nil {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			st, qerr := s.Query()
			if qerr != nil || st.State == svc.Stopped {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("eliminar servicio: %w", err)
	}
	return nil
}
