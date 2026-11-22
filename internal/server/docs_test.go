package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestHTTPDocsServesManual(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	for _, path := range []string{"/docs", "/docs/", "/docs/manual.html", "/help", "/"} {
		resp, body := httpDo(t, srv, http.MethodGet, path, "", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s estado = %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s Content-Type = %q", path, ct)
		}
		text := string(body)
		if !strings.Contains(text, "Spooler Fiscal") || !strings.Contains(text, "</html>") {
			t.Fatalf("%s no devolvió el manual (%d bytes)", path, len(body))
		}
	}
}

func TestHTTPDocsIncludesEveryLanguage(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	_, body := httpDo(t, srv, http.MethodGet, "/docs", "", nil, nil)
	text := string(body)
	for _, want := range []string{
		"cURL (bash)", "PowerShell", "PHP 8+", "Go", "Python 3.10+", "Node.js 18+", "Rust 1.75+",
		"/v1/invoices/structured", "Idempotency-Key", "command_terminator",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("el manual no menciona %q", want)
		}
	}
}

func TestHTTPDocsServesDiccionario(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	for _, path := range []string{"/docs/diccionario", "/docs/diccionario/", "/docs/diccionario/index.html"} {
		resp, body := httpDo(t, srv, http.MethodGet, path, "", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s estado = %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s Content-Type = %q", path, ct)
		}
		text := string(body)
		if !strings.Contains(text, "Diccionario del código") || !strings.Contains(text, "</html>") {
			t.Fatalf("%s no devolvió el diccionario (%d bytes)", path, len(body))
		}
	}
}

func TestHTTPAdminPanel(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	for _, path := range []string{"/admin", "/admin/"} {
		resp, body := httpDo(t, srv, http.MethodGet, path, "", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s estado = %d", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s Content-Type = %q", path, ct)
		}
		text := string(body)
		for _, want := range []string{
			"Spooler Fiscal", "Cancelar", "Reintentar", "/v1/jobs/",
			"sessionStorage", "documents?limit=",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("el panel no contiene %q", want)
			}
		}
		// La página es pública: no debe incluir el token.
		if strings.Contains(text, "tok-rest") {
			t.Fatal("el panel no debe incluir el token")
		}
	}
}

func TestHTTPDocsServesOpenAPI(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	resp, body := httpDo(t, srv, http.MethodGet, "/docs/openapi.yaml", "", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("estado = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(string(body), "openapi: 3.0.3") ||
		!strings.Contains(string(body), "/v1/invoices/structured") {
		t.Fatalf("OpenAPI inesperado (%d bytes)", len(body))
	}
}
