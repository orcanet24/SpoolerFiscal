// Command agent es el servicio de cada estación: expone la impresora fiscal en
// un puerto COM virtual y reenvía los bytes al spooler central.
//
//	agent.exe -config configs\agent.yaml -console
//	agent.exe -install -config C:\SpoolerFiscal\configs\agent.yaml
//	agent.exe -install-com0com -profit-port COM3 -agent-port COM4
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"spoolerfiscal/internal/agent"
	"spoolerfiscal/internal/comvirt"
	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/logging"
	"spoolerfiscal/internal/servicehost"
)

func main() {
	// Detectar -install-com0com manualmente ANTES de servicehost
	installCom0com := false
	profitPort := "COM3"
	agentPort := "COM4"
	configPath := "configs\\agent.yaml"

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-install-com0com":
			installCom0com = true
		case "-profit-port":
			if i+1 < len(args) {
				profitPort = args[i+1]
				i++
			}
		case "-agent-port":
			if i+1 < len(args) {
				agentPort = args[i+1]
				i++
			}
		case "-config":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		}
	}

	if installCom0com {
		if err := runInstallCom0com(configPath, profitPort, agentPort); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		return
	}

	// servicehost.Main maneja el resto de flags (-help, -docs, -install, -console, etc.)
	servicehost.Main(servicehost.App{
		Name:          "SpoolerFiscalAgent",
		DisplayName:   "Spooler Fiscal (agente de estación)",
		Description:   "Emula la impresora fiscal en un puerto COM local y reenvía los comandos al spooler central.",
		DefaultConfig: `configs\agent.yaml`,
		Usage:         agentUsage,
		Docs:          agentDocs,
		Run:           run,
	})
}

func run(ctx context.Context, configPath string) error {
	cfg, err := config.LoadAgent(configPath)
	if err != nil {
		return err
	}
	logger, closeLog, err := logging.Setup(cfg.Logging, "logs/agent.log")
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()

	// El YAML contiene el token compartido con el spooler.
	if err := config.ProtectConfigFile(configPath); err != nil {
		logger.Warn("no se restringieron los permisos del archivo de configuración", "error", err)
	}

	logger.Info("iniciando agente fiscal", "version", agent.Version,
		"estacion", cfg.StationID, "config", configPath)

	ag := agent.New(cfg, logger)
	if err := ag.Start(ctx); err != nil {
		return err
	}
	defer ag.Close()

	go logMetrics(ctx, logger, ag)

	// Bucle de puerto: si el COM virtual desaparece (driver reiniciado, USB
	// desconectado), el servicio lo reabre con backoff sin detenerse.
	delay := durationOr(cfg.Bridge.ReopenDelayMS, time.Second)
	maxDelay := durationOr(cfg.Bridge.ReopenMaxDelayMS, 30*time.Second)

	for {
		if ctx.Err() != nil {
			return nil
		}
		runErr := runBridge(ctx, cfg, ag, logger)
		if ctx.Err() != nil {
			return nil
		}
		logger.Warn("puente COM detenido; se reintenta abrir el puerto",
			"puerto", cfg.VirtualCOM.Name, "error", runErr, "espera", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

func runBridge(ctx context.Context, cfg config.Agent, ag *agent.Agent, logger *slog.Logger) error {
	port, err := comvirt.OpenPort(cfg.VirtualCOM)
	if err != nil {
		return err
	}
	logger.Info("COM virtual abierto", "puerto", cfg.VirtualCOM.Name,
		"baudios", cfg.VirtualCOM.BaudRate, "trama", "8N1")

	bridge, err := comvirt.NewBridge(port, ag.Forward, cfg, logger)
	if err != nil {
		_ = port.Close()
		return err
	}
	defer func() { _ = port.Close() }()
	return bridge.Run(ctx)
}

func logMetrics(ctx context.Context, logger *slog.Logger, ag *agent.Agent) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frames, errs, retries := ag.Stats()
			logger.Info("métricas del agente", "tramas", frames, "errores", errs,
				"reintentos_red", retries)
		}
	}
}

func durationOr(ms int, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}

// runInstallCom0com descarga, instala com0com silenciosamente y crea el par COM.
func runInstallCom0com(configPath, profitPort, agentPort string) error {
	// Leer virtual_com del config si existe (para usar puertos del YAML)
	cfg, err := config.LoadAgent(configPath)
	if err == nil && cfg.VirtualCOM.Name != "" {
		// cfg.VirtualCOM.Name es el puerto del AGENTE; el de Profit es la pareja
		// Intentar inferir: si config dice COM4, asumimos COM3 para Profit
		agentPort = cfg.VirtualCOM.Name
		// Heurística simple: COM4->COM3, COM12->COM11, etc.
		if len(agentPort) > 3 {
			if n := agentPort[3:]; n != "" {
				if _, err := fmt.Sscanf(n, "%d", new(int)); err == nil {
					// Es COM + número
				}
			}
		}
	}

	// Solo Windows
	if runtime.GOOS != "windows" {
		return fmt.Errorf("com0com solo está disponible en Windows")
	}

	// Verificar admin
	if !isAdmin() {
		return fmt.Errorf("se requieren permisos de administrador (ejecutar PowerShell/CMD como Admin)")
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("Instalando com0com...", "profit_port", profitPort, "agent_port", agentPort)

	// 1. Verificar si setupc.exe ya existe
	setupcPath := findSetupC()
	if setupcPath == "" {
		// 2. Descargar instalador
		installerPath, err := downloadCom0comInstaller(log)
		if err != nil {
			return fmt.Errorf("descargar com0com: %w", err)
		}
		// 3. Instalar silencioso
		log.Info("Instalando com0com (silencioso)...")
		if err := runSilent(installerPath, "/S"); err != nil {
			return fmt.Errorf("instalar com0com: %w", err)
		}
		// 4. Buscar setupc.exe después de instalar
		setupcPath = findSetupC()
		if setupcPath == "" {
			return fmt.Errorf("com0com instalado pero no se encuentra setupc.exe (¿reinició la PC?)")
		}
	}

	// 5. Crear par COM
	log.Info("Creando par COM virtual...", "setupc", setupcPath)
	args := []string{"install", fmt.Sprintf("PortName=%s", profitPort), fmt.Sprintf("PortName=%s", agentPort)}
	if err := runCommand(setupcPath, args...); err != nil {
		return fmt.Errorf("crear par COM: %w", err)
	}

	log.Info("¡Listo! Par COM creado:", "profit_plus", profitPort, "agente", agentPort)
	log.Info("Verifique en Administrador de dispositivos > Puertos (COM y LPT)")
	log.Info("Ahora configure Profit Plus en " + profitPort + " y agent.yaml con virtual_com.name: " + agentPort)
	return nil
}

func findSetupC() string {
	// Rutas típicas de instalación de com0com
	paths := []string{
		`C:\Program Files (x86)\com0com\setupc.exe`,
		`C:\Program Files\com0com\setupc.exe`,
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Buscar en PATH
	if p, err := exec.LookPath("setupc.exe"); err == nil {
		return p
	}
	return ""
}

func downloadCom0comInstaller(log *slog.Logger) (string, error) {
	url := "https://github.com/com0com/com0com/releases/download/v2.2.2.0/com0com-2.2.2.0-x64.exe"
	dest := filepath.Join(os.TempDir(), "com0com-installer.exe")

	log.Info("Descargando com0com...", "url", url)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", err
	}
	log.Info("Descargado", "archivo", dest)
	return dest, nil
}

func runSilent(exe, args string) error {
	cmd := exec.Command(exe, args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runCommand(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func isAdmin() bool {
	// En Windows, verificar si el proceso está elevado
	cmd := exec.Command("net", "session")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}
