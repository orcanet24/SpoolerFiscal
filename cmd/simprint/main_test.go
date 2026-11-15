package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPanelForwardEntregaRespuesta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/command" {
			t.Errorf("ruta: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("método: %s", r.Method)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("cuerpo: %v", err)
		}
		if body["brand"] != "tfhka" || body["command"] != "101" {
			t.Errorf("cuerpo: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"command":"101","response":"0|Factura 3|Total: 10.00|Impreso:3","seq":10}`)
	}))
	defer srv.Close()

	got := panelForward(context.Background(), srv.URL, "tfhka", "101")
	if got != "0|Factura 3|Total: 10.00|Impreso:3" {
		t.Fatalf("respuesta: %q", got)
	}
}

func TestPanelForwardSinPanelDevuelveErrorFiscal(t *testing.T) {
	got := panelForward(context.Background(), "http://127.0.0.1:1", "tfhka", "101")
	if !strings.HasPrefix(got, "ERROR|0x") {
		t.Fatalf("se esperaba un error fiscal, llegó %q", got)
	}
}
