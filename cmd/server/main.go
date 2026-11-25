// Command server es el spooler fiscal central: recibe los comandos de todas las
// estaciones y los ejecuta en orden sobre la impresora fiscal compartida.
//
//	server.exe -config configs\server.yaml -console
//	server.exe -install -config C:\Proyectos\GO\SpoolerFiscal\configs\server.yaml
package main

import (
	"context"
	"path/filepath"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/logging"
	"spoolerfiscal/internal/server"
	"spoolerfiscal/internal/servicehost"
)

func main() {
	servicehost.Main(servicehost.App{
		Name:          "SpoolerFiscalServer",
		DisplayName:   "Spooler Fiscal Central",
		Description:   "Serializa la emisión fiscal de todas las estaciones sobre una única impresora fiscal y mantiene el correlativo centralizado.",
		DefaultConfig: `configs\server.yaml`,
		Usage:         serverUsage,
		Docs:          serverDocs,
		Run:           run,
	})
}

func run(ctx context.Context, configPath string) error {
	cfg, err := config.LoadServer(configPath)
	if err != nil {
		logging.ReportConfigError(err, configPath, "logs/server.log")
		return err
	}
	logger, closeLog, err := logging.Setup(cfg.Logging, "logs/server.log")
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()

	// El YAML contiene el token compartido y data\ la auditoría fiscal.
	if err := config.ProtectConfigFile(configPath); err != nil {
		logger.Warn("no se restringieron los permisos del archivo de configuración", "error", err)
	}
	if dir := filepath.Dir(cfg.Database.Path); dir != "." && dir != "" {
		if err := config.ProtectDataDir(dir); err != nil {
			logger.Warn("no se restringieron los permisos del directorio de datos", "error", err)
		}
	}

	logger.Info("iniciando spooler fiscal", "version", server.Version, "config", configPath)

	rt, err := server.NewRuntime(cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = rt.Close() }()

	return rt.Run(ctx)
}
