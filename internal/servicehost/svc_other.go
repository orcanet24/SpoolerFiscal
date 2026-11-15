//go:build !windows

package servicehost

import "errors"

func isWindowsService() bool { return false }

func runService(app App, configPath string) error {
	runConsole(app, configPath)
	return nil
}

func runDebug(app App, configPath string) error {
	runConsole(app, configPath)
	return nil
}

func installService(app App, configPath string) error {
	return errors.New("la instalación como servicio sólo está disponible en Windows")
}

func uninstallService(app App) error {
	return errors.New("la desinstalación de servicio sólo está disponible en Windows")
}
