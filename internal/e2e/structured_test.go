package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/server"
	"spoolerfiscal/internal/testutil"
)

// TestStructuredInvoiceAgainstSimulator emite una factura compilada desde
// ítems JSON contra el simulador TFHKA real. Requiere el emulador corriendo:
//
//	simuladorfiscal.exe serve --model tfhka --addr 127.0.0.1:2011
//	SPOOLER_E2E_PRINTER_ADDR=127.0.0.1:2011 go test ./internal/e2e -run TestStructuredInvoiceAgainstSimulator -v
func TestStructuredInvoiceAgainstSimulator(t *testing.T) {
	addr := os.Getenv("SPOOLER_E2E_PRINTER_ADDR")
	if addr == "" {
		t.Skip("defina SPOOLER_E2E_PRINTER_ADDR para probar contra el simulador")
	}

	cfg := config.DefaultServer()
	dir := t.TempDir()
	cfg.Token = "token-estructurada-e2e"
	cfg.Printer.Transport = "tcp"
	cfg.Printer.TCPAddress = addr
	cfg.Database.Path = filepath.Join(dir, "spooler.db")
	cfg.Logging.File = filepath.Join(dir, "server.log")
	cfg.Logging.Console = false
	cfg.HTTP.Listen = "127.0.0.1:0"

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt, err := server.NewRuntime(cfg, logger)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	defer func() {
		cancel()
		<-done
		_ = rt.Close()
	}()

	base := "http://" + rt.Addr()
	client := &http.Client{Timeout: 60 * time.Second}
	waitHealthy(t, client, base+"/v1/health")

	payload := map[string]any{
		"station":    "ECOM-E2E",
		"client_ref": "pedido-estructurado",
		"items": []map[string]any{
			{"description": "CAFE", "quantity": 1, "unit_price": 10.00, "tax": "general"},
			{"description": "PAN", "quantity": 2, "unit_price": 5.00, "tax": "reduced"},
		},
		"totals": map[string]any{"total": 20.00, "subtotal": 17.88, "tax": 2.12},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/invoices/structured", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "estructurada-e2e-1")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/invoices/structured: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, data)
	}

	var out struct {
		Completed     bool     `json:"completed"`
		InvoiceNumber int64    `json:"invoice_number"`
		CompiledLines []string `json:"compiled_lines"`
		Totals        struct {
			Total float64 `json:"total"`
		} `json:"totals"`
		Document *struct {
			Number       int64  `json:"number"`
			ResponseText string `json:"response_text"`
		} `json:"document"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("JSON de respuesta: %v (%s)", err, data)
	}
	if !out.Completed || out.InvoiceNumber <= 0 {
		t.Fatalf("factura incompleta: %s", data)
	}
	want := []string{"!   1.000     10.00CAFE", "\"   2.000      5.00PAN", "101"}
	if len(out.CompiledLines) != len(want) {
		t.Fatalf("líneas compiladas = %q", out.CompiledLines)
	}
	for i := range want {
		if out.CompiledLines[i] != want[i] {
			t.Fatalf("línea %d = %q, se esperaba %q", i, out.CompiledLines[i], want[i])
		}
	}
	if math.Abs(out.Totals.Total-20.00) > 0.001 {
		t.Fatalf("total calculado = %.2f", out.Totals.Total)
	}
	if out.Document == nil || !strings.Contains(out.Document.ResponseText, "Total: 20.00") {
		t.Fatalf("la impresora no confirmó el total: %+v", out.Document)
	}
	t.Logf("factura %d emitida contra %s; compiladas %d líneas, total %.2f; impresora: %s",
		out.InvoiceNumber, addr, len(out.CompiledLines), out.Totals.Total, out.Document.ResponseText)
}

// TestStructuredFullSaleEndToEnd emite una venta completa (cliente, ítems,
// descuento y pago) por el circuito COMP virtual → agente → spooler →
// impresora emulada, y verifica la secuencia exacta de comandos TFHKA.
func TestStructuredFullSaleEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "spooler.db")
	simLn, sim, err := testutil.StartTCPSim(ctx, "127.0.0.1:0", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartTCPSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()

	h := startServer(t, dbPath, simLn.Addr().String())
	base := "http://" + h.rt.Addr()
	client := &http.Client{Timeout: 30 * time.Second}
	waitHealthy(t, client, base+"/v1/health")

	payload := map[string]any{
		"station":    "ECOM-FULL",
		"client_ref": "pedido-full",
		"customer": map[string]any{
			"rif": "J-12345678-9", "name": "CLIENTE C.A.",
			"address": "Av. 1", "phone": "0212",
		},
		"items": []map[string]any{
			{"description": "CAFE", "quantity": 1, "unit_price": 10.00, "tax": "general"},
		},
		"discount": 1.00,
		"payments": []map[string]any{
			{"method": "tarjeta", "amount": 9.00},
		},
		"totals": map[string]any{"total": 9.00},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/invoices/structured", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "full-sale-e2e-1")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST structured: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, data)
	}
	var out struct {
		Completed     bool     `json:"completed"`
		InvoiceNumber int64    `json:"invoice_number"`
		CompiledLines []string `json:"compiled_lines"`
		Totals        struct {
			Total    float64 `json:"total"`
			Discount float64 `json:"discount"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("JSON: %v (%s)", err, data)
	}
	if !out.Completed || out.InvoiceNumber != 1 {
		t.Fatalf("factura incompleta: %s", data)
	}
	if math.Abs(out.Totals.Total-9.00) > 0.001 || out.Totals.Discount != 1.00 {
		t.Fatalf("totales = %+v", out.Totals)
	}

	want := []string{
		"iR*J-12345678-9",
		"iS*CLIENTE C.A.",
		"i01Av. 1",
		"i020212",
		"!   1.000     10.00CAFE",
		"3",
		"7001.00",
		"103tarjeta",
		"1009.00",
		"101",
	}
	// Las sondas S1 de conciliación son de sólo lectura: se descartan para
	// comparar la secuencia fiscal.
	var got []string
	for _, c := range sim.Commands() {
		if c == "S1" {
			continue
		}
		got = append(got, c)
	}
	if len(got) != len(want) {
		t.Fatalf("comandos en la impresora = %d (%q)", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("comando %d = %q, se esperaba %q", i, got[i], want[i])
		}
	}
	if sim.InvoiceCount() != 1 {
		t.Fatalf("la impresora emitió %d facturas", sim.InvoiceCount())
	}
}

func waitHealthy(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("la API REST no respondió en %s", url)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
