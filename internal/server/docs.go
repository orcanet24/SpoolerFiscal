package server

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"
)

// El manual y el contrato viajan dentro del binario: /docs funciona aunque la
// instalación no incluya la carpeta docs.
//
//go:embed web/manual.html
var embeddedManual []byte

//go:embed web/openapi.yaml
var embeddedOpenAPI []byte

//go:embed web/diccionario.html
var embeddedDiccionario []byte

//go:embed web/admin.html
var embeddedAdmin []byte

// handleAdmin sirve el panel de operación. La página no contiene secretos: el
// token se pide en el navegador y sólo viaja a la API en cada llamada.
func (a *HTTPAPI) handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(embeddedAdmin)
}

// handleDocsRoot redirige la raíz y /help al manual.
func (a *HTTPAPI) handleDocsRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/docs", http.StatusFound)
}

// handleDocs sirve el manual. Si el proyecto está al lado del ejecutable (o se
// ejecuta desde la raíz), se usa el archivo en disco para permitir editarlo sin
// recompilar; si no, el embebido.
func (a *HTTPAPI) handleDocs(w http.ResponseWriter, r *http.Request) {
	body := embeddedManual
	for _, path := range docCandidates("docs/manual.html") {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			body = b
			break
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleDiccionario sirve el diccionario navegable del código.
func (a *HTTPAPI) handleDiccionario(w http.ResponseWriter, r *http.Request) {
	body := embeddedDiccionario
	for _, path := range docCandidates("docs/diccionario/index.html") {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			body = b
			break
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleOpenAPI sirve el contrato para Postman, Insomnia o generadores.
func (a *HTTPAPI) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	body := embeddedOpenAPI
	for _, path := range docCandidates("api/openapi.yaml") {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			body = b
			break
		}
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// docCandidates devuelve la ruta relativa al directorio actual y la relativa al
// ejecutable (instalaciones donde docs\ está junto al binario).
func docCandidates(rel string) []string {
	out := []string{rel}
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), filepath.FromSlash(rel)))
	}
	return out
}
