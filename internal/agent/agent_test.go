package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"spoolerfiscal/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testAgent(t *testing.T, url string) *Agent {
	t.Helper()
	cfg := config.DefaultAgent()
	cfg.StationID = "CAJA-01"
	cfg.Server.Address = url
	cfg.Server.Token = "secreto"
	cfg.Server.RequestTimeoutMS = 2000
	cfg.Bridge.MaxSendBackoffMS = 20
	ag := New(cfg, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		ag.Close()
	})
	if err := ag.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return ag
}

func TestForwardPostsRawCommand(t *testing.T) {
	var gotAuth, gotIdem, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"station":        "CAJA-01",
			"session":        "api:CAJA-01",
			"response_base64": base64.StdEncoding.EncodeToString([]byte("0|OK\r\n")),
		})
	}))
	defer srv.Close()

	ag := testAgent(t, srv.URL)
	resp, err := ag.Forward(context.Background(), []byte("S1\r\n"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if string(resp) != "0|OK\r\n" {
		t.Fatalf("respuesta = %q", resp)
	}
	if gotAuth != "Bearer secreto" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotIdem == "" {
		t.Fatal("se esperaba Idempotency-Key")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("cuerpo inválido: %v", err)
	}
	if body["command_base64"] != base64.StdEncoding.EncodeToString([]byte("S1\r\n")) {
		t.Fatalf("command_base64 = %v", body["command_base64"])
	}
	if body["station"] != "CAJA-01" {
		t.Fatalf("station = %v", body["station"])
	}
}

func TestForwardRetriesTransientStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, `{"error":{"code":"printer_unavailable","message":"impresora caída"}}`, http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response_base64": base64.StdEncoding.EncodeToString([]byte("0|Factura 1|\r\n")),
		})
	}))
	defer srv.Close()

	ag := testAgent(t, srv.URL)
	resp, err := ag.Forward(context.Background(), []byte("!   1.000     10.00CAFE\r\n"))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if string(resp) != "0|Factura 1|\r\n" {
		t.Fatalf("respuesta = %q", resp)
	}
	if calls.Load() < 2 {
		t.Fatalf("no se reintentó: llamadas = %d", calls.Load())
	}
}

func TestForwardDoesNotRetryAuthError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"code":"unauthorized","message":"token inválido"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	ag := testAgent(t, srv.URL)
	if _, err := ag.Forward(context.Background(), []byte("S1\r\n")); err == nil {
		t.Fatal("se esperaba error de autenticación")
	}
	if calls.Load() != 1 {
		t.Fatalf("un error 401 no debe reintentarse: llamadas = %d", calls.Load())
	}
}

func TestForwardUsesTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_ = json.NewEncoder(w).Encode(map[string]any{"response_base64": base64.StdEncoding.EncodeToString([]byte("0|OK\r\n"))})
	}))
	defer srv.Close()

	ag := testAgent(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := ag.Forward(ctx, []byte("S1\r\n")); err == nil {
		t.Fatal("se esperaba timeout")
	}
}
