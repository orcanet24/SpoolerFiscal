// Package servicehost ejecuta un binario como servicio de Windows o en consola.
package servicehost

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"spoolerfiscal/internal/config"
)

// App describe el servicio a ejecutar.
type App struct {
	Name          string
	DisplayName   string
	Description   string
	DefaultConfig string
	// Usage se imprime con -help (y con -h).
	Usage string
	// Docs se imprime con -docs: guía rápida y dónde está el manual.
	Docs string
	Run  func(ctx context.Context, configPath string) error
}

// Main parsea los flags, instala/desinstala o ejecuta la aplicación.
func Main(app App) {
	if err := config.LoadExecutableDotEnv(); err != nil {
		fail(err)
	}

	fs := flag.NewFlagSet(app.Name, flag.ExitOnError)
	configPath := fs.String("config", app.DefaultConfig, "ruta del archivo de configuración YAML")
	console := fs.Bool("console", false, "ejecutar en primer plano en lugar de como servicio")
	install := fs.Bool("install", false, "instalar el servicio de Windows (inicio automático)")
	uninstall := fs.Bool("uninstall", false, "detener y desinstalar el servicio de Windows")
	debugMode := fs.Bool("debug", false, "depurar el servicio adjunto a la consola")
	help := fs.Bool("help", false, "mostrar la ayuda y salir")
	docs := fs.Bool("docs", false, "mostrar la guía de uso y la ruta del manual")
	if app.Usage != "" {
		fs.Usage = func() { fmt.Fprint(os.Stdout, app.Usage) }
	}
	_ = fs.Parse(os.Args[1:])
	if *help {
		fmt.Print(app.Usage)
		return
	}
	if *docs {
		fmt.Print(app.Docs)
		return
	}
	configProvided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configProvided = true
		}
	})
	if !configProvided && !filepath.IsAbs(*configPath) {
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), *configPath)
			if _, err := os.Stat(candidate); err == nil {
				*configPath = candidate
			}
		}
	}

	switch {
	case *install:
		path, err := filepath.Abs(*configPath)
		if err != nil {
			fail(err)
		}
		if err := installService(app, path); err != nil {
			fail(err)
		}
		exe, _ := os.Executable()
		fmt.Printf("Servicio %s instalado (inicio automático).\n", app.Name)
		fmt.Printf("  Binario:        %s\n", exe)
		fmt.Printf("  Configuración:  %s\n", path)
		fmt.Printf("  Iniciar:        sc start %s\n", app.Name)
		fmt.Printf("  Estado:         sc query %s\n", app.Name)
	case *uninstall:
		if err := uninstallService(app); err != nil {
			fail(err)
		}
		fmt.Printf("Servicio %s detenido y desinstalado.\n", app.Name)
	case *debugMode:
		if err := runDebug(app, *configPath); err != nil {
			fail(err)
		}
	case *console || !isWindowsService():
		runConsole(app, *configPath)
	default:
		if err := runService(app, *configPath); err != nil {
			fail(err)
		}
	}
}

// runConsole ejecuta la aplicación con Ctrl+C como señal de parada.
func runConsole(app App, configPath string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, configPath); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}
