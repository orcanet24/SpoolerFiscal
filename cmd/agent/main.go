// Command agent es el servicio de cada estación: expone la impresora fiscal en
// un puerto COM virtual y reenvía los bytes al spooler central.
//
//	agent.exe -config configs\agent.yaml -console
//	agent.exe -install -config C:\SpoolerFiscal\configs\agent.yaml
package main

import (
	"context"
	"log/slog"
	"time"

	"spoolerfiscal/internal/agent"
	"spoolerfiscal/internal/comvirt"
	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/logging"
	"spoolerfiscal/internal/servicehost"
)

func main() {
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
