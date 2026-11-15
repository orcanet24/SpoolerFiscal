package config

// ProtectConfigFile restringe el acceso al YAML de configuración, que contiene
// el token compartido. En Windows deja la ACL en SYSTEM, Administradores y el
// usuario que ejecuta el proceso. En otros sistemas no hace nada.
func ProtectConfigFile(path string) error { return protectPath(path, false) }

// ProtectDataDir restringe el acceso al directorio de la base de auditoría.
func ProtectDataDir(path string) error { return protectPath(path, true) }
