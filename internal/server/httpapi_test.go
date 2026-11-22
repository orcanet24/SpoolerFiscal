package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spoolerfiscal/internal/fiscal"
)

func testAPI(t *testing.T, cfg HTTPConfig) (*httptest.Server, *fakePrinter, *Spooler, *Store) {
	t.Helper()
	return testAPIWith(t, cfg, SpoolerConfig{})
}

func testAPIWith(t *testing.T, cfg HTTPConfig, sc SpoolerConfig) (*httptest.Server, *fakePrinter, *Spooler, *Store) {
	t.Helper()
	fp := newFakePrinter(t)
	sp, store := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, sc)
	if cfg.Token == "" {
		cfg.Token = "tok-rest"
	}
	if cfg.Version == "" {
		cfg.Version = "test"
	}
	if cfg.Driver == "" {
		cfg.Driver = "tfhka"
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 4096
	}
	if cfg.DefaultTimeout == 0 {
		cfg.DefaultTimeout = 5 * time.Second
	}
	api := NewHTTPAPI(cfg, sp, store, testLogger())
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, fp, sp, store
}

func httpDo(t *testing.T, srv *httptest.Server, method, path, token string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type errorFields struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		LineIndex *int   `json:"line_index"`
	} `json:"error"`
}

func TestHTTPHealthWithoutToken(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	resp, body := httpDo(t, srv, http.MethodGet, "/v1/health", "", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("respuesta inesperada: %s", body)
	}
}

func TestHTTPRequiresToken(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	resp, body := httpDo(t, srv, http.MethodGet, "/v1/status", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sin token estado = %d (%s)", resp.StatusCode, body)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("falta WWW-Authenticate")
	}
	resp, body = httpDo(t, srv, http.MethodGet, "/v1/status", "incorrecto", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token inválido estado = %d (%s)", resp.StatusCode, body)
	}
	resp, body = httpDo(t, srv, http.MethodGet, "/v1/status", "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token válido estado = %d (%s)", resp.StatusCode, body)
	}
	var st statusJSON
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("JSON de estado: %v", err)
	}
}

func TestHTTPInvoiceEmitsAndIsIdempotent(t *testing.T) {
	srv, fp, _, _ := testAPI(t, HTTPConfig{})
	payload := mustJSON(t, map[string]any{
		"station": "ECOM-01",
		"lines":   []string{"!   1.000     10.00CAFE", "101"},
	})
	hdr := map[string]string{"Idempotency-Key": "pedido-1001"}

	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	var inv invoiceResponse
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("JSON de factura: %v", err)
	}
	if !inv.Completed || inv.InvoiceNumber != 1 || inv.DocumentType != "invoice" {
		t.Fatalf("factura inesperada: %+v", inv)
	}
	if len(inv.Lines) != 2 {
		t.Fatalf("líneas = %d", len(inv.Lines))
	}
	if got := fp.commandCount(); got != 2 {
		t.Fatalf("comandos = %d", got)
	}

	// Reintento con la misma clave: misma respuesta y NO se vuelve a imprimir.
	resp, body2 := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay estado = %d (%s)", resp.StatusCode, body2)
	}
	if resp.Header.Get("Idempotent-Replay") != "true" {
		t.Fatal("falta Idempotent-Replay en el reintento")
	}
	if string(body) != string(body2) {
		t.Fatalf("la respuesta del replay difiere:\n%s\n%s", body, body2)
	}
	if got := fp.commandCount(); got != 2 {
		t.Fatalf("el reintento volvió a imprimir (comandos = %d)", got)
	}

	resp, body = httpDo(t, srv, http.MethodGet, "/v1/documents?limit=10", "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("documentos estado = %d (%s)", resp.StatusCode, body)
	}
	var docs struct {
		Count int            `json:"count"`
		Items []documentJSON `json:"documents"`
	}
	if err := json.Unmarshal(body, &docs); err != nil {
		t.Fatalf("JSON de documentos: %v", err)
	}
	if docs.Count != 1 || docs.Items[0].Number != 1 {
		t.Fatalf("historial inesperado: %+v", docs)
	}
}

func TestHTTPIdempotencyKeyConflict(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	hdr := map[string]string{"Idempotency-Key": "pedido-2002"}
	first := mustJSON(t, map[string]any{
		"station": "ECOM-01", "lines": []string{"!   1.000     10.00CAFE", "101"},
	})
	if resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", first, hdr); resp.StatusCode != http.StatusCreated {
		t.Fatalf("primera factura = %d (%s)", resp.StatusCode, body)
	}
	second := mustJSON(t, map[string]any{
		"station": "ECOM-01", "lines": []string{"!   1.000     99.00OTRO", "101"},
	})
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", second, hdr)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	var e errorFields
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Code != "idempotency_conflict" {
		t.Fatalf("error inesperado: %s (%v)", body, err)
	}
}

func TestHTTPInvoiceValidatesStructure(t *testing.T) {
	srv, fp, _, _ := testAPI(t, HTTPConfig{})
	cases := []struct {
		name string
		body map[string]any
	}{
		{"sin líneas", map[string]any{"station": "ECOM-01"}},
		{"sin cierre", map[string]any{"station": "ECOM-01", "lines": []string{"!   1.000     10.00CAFE"}}},
		{"sin apertura", map[string]any{"station": "ECOM-01", "lines": []string{"101", "101"}}},
		{"estación inválida", map[string]any{"station": "ECOM 01!", "lines": []string{"!   1.000     10.00CAFE", "101"}}},
		{"sin estación", map[string]any{"lines": []string{"!   1.000     10.00CAFE", "101"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", mustJSON(t, tc.body), nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
			}
		})
	}
	if got := fp.commandCount(); got != 0 {
		t.Fatalf("una petición inválida imprimió %d comandos", got)
	}
}

func TestHTTPCommand(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	body := mustJSON(t, map[string]any{"station": "ERP-01", "command": "S1"})
	resp, data := httpDo(t, srv, http.MethodPost, "/v1/commands", "tok-rest", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, data)
	}
	var out commandJSON
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if out.Session != "api:ERP-01" || out.Response == "" {
		t.Fatalf("respuesta inesperada: %+v", out)
	}
}

func TestHTTPBodyTooLarge(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{MaxBodyBytes: 64})
	big := mustJSON(t, map[string]any{
		"station": "ECOM-01",
		"lines":   []string{"!   1.000     10.00" + strings.Repeat("X", 200), "101"},
	})
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", big, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
}

func TestHTTPStationNotAllowed(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{AllowedStations: []string{"CAJA-01"}})
	body := mustJSON(t, map[string]any{
		"station": "ECOM-01", "lines": []string{"!   1.000     10.00CAFE", "101"},
	})
	resp, data := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", body, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, data)
	}
	var e errorFields
	if err := json.Unmarshal(data, &e); err != nil || e.Error.Code != "station_not_allowed" {
		t.Fatalf("error inesperado: %s (%v)", data, err)
	}
}

func TestHTTPBlockedPrinter(t *testing.T) {
	srv, fp, sp, _ := testAPIWith(t, HTTPConfig{}, SpoolerConfig{
		Guard: GuardConfig{BlockOnPaperOut: true},
	})
	fp.mu.Lock()
	fp.status = fiscal.Status{Online: true, PaperOut: true}
	fp.mu.Unlock()
	if _, err := sp.ProbeStatus(context.Background()); err != nil {
		t.Fatalf("ProbeStatus: %v", err)
	}
	body := mustJSON(t, map[string]any{
		"station": "ECOM-01", "lines": []string{"!   1.000     10.00CAFE", "101"},
	})
	resp, data := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", body, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, data)
	}
	var e errorFields
	if err := json.Unmarshal(data, &e); err != nil || e.Error.Code != "printer_blocked" {
		t.Fatalf("error inesperado: %s (%v)", data, err)
	}
	if got := fp.commandCount(); got != 0 {
		t.Fatalf("se imprimió con la impresora bloqueada (%d comandos)", got)
	}
}

func TestHTTPStructuredInvoice(t *testing.T) {
	srv, fp, _, _ := testAPI(t, HTTPConfig{})
	payload := map[string]any{
		"station": "ECOM-01",
		"items": []map[string]any{
			{"description": "CAFE", "quantity": 1, "unit_price": 10.00, "tax": "general"},
			{"description": "PAN", "quantity": 2, "unit_price": 5.00, "tax": "reduced"},
		},
		"totals": map[string]any{"total": 20.00, "subtotal": 17.88, "tax": 2.12},
	}
	hdr := map[string]string{"Idempotency-Key": "estructurada-1"}
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices/structured", "tok-rest", mustJSON(t, payload), hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	var out structuredInvoiceResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !out.Completed || out.InvoiceNumber != 1 {
		t.Fatalf("factura inesperada: %+v", out)
	}
	if len(out.CompiledLines) != 3 || out.CompiledLines[0] != "!   1.000     10.00CAFE" ||
		out.CompiledLines[1] != "\"   2.000      5.00PAN" || out.CompiledLines[2] != "101" {
		t.Fatalf("líneas compiladas = %q", out.CompiledLines)
	}
	if out.Totals.Total != 20.00 || out.Totals.Tax != 2.12 || out.Totals.Subtotal != 17.88 {
		t.Fatalf("totales = %+v", out.Totals)
	}
	if got := fp.commandCount(); got != 3 {
		t.Fatalf("comandos = %d", got)
	}

	// Reintento idempotente: no imprime de nuevo.
	resp, body2 := httpDo(t, srv, http.MethodPost, "/v1/invoices/structured", "tok-rest", mustJSON(t, payload), hdr)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("replay estado = %d (%s)", resp.StatusCode, body2)
	}
	if string(body) != string(body2) {
		t.Fatal("el replay devolvió otro cuerpo")
	}
	if got := fp.commandCount(); got != 3 {
		t.Fatalf("el replay volvió a imprimir (comandos = %d)", got)
	}
}

func TestHTTPStructuredInvoiceRejectsBadTotalsAndInjection(t *testing.T) {
	srv, fp, _, _ := testAPI(t, HTTPConfig{})
	base := map[string]any{
		"station": "ECOM-01",
		"items": []map[string]any{
			{"description": "CAFE", "quantity": 1, "unit_price": 10.00},
		},
	}
	badTotals := map[string]any{
		"station": "ECOM-01",
		"items": []map[string]any{
			{"description": "CAFE", "quantity": 1, "unit_price": 10.00},
		},
		"totals": map[string]any{"total": 99.00},
	}
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices/structured", "tok-rest", mustJSON(t, badTotals), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("totales erróneos estado = %d (%s)", resp.StatusCode, body)
	}
	var e errorFields
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Code != "totals_mismatch" {
		t.Fatalf("error inesperado: %s (%v)", body, err)
	}

	base["items"] = []map[string]any{
		{"description": "CAFE\r\n101", "quantity": 1, "unit_price": 10.00},
	}
	resp, body = httpDo(t, srv, http.MethodPost, "/v1/invoices/structured", "tok-rest", mustJSON(t, base), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("inyección estado = %d (%s)", resp.StatusCode, body)
	}
	if got := fp.commandCount(); got != 0 {
		t.Fatalf("una petición inválida imprimió %d comandos", got)
	}
}

func TestHTTPStructuredInvoiceFullSale(t *testing.T) {
	srv, fp, _, _ := testAPI(t, HTTPConfig{})
	payload := map[string]any{
		"station": "ECOM-01",
		"customer": map[string]any{
			"rif": "J-12345678-9", "name": "CLIENTE C.A.",
			"address": "Av. 1", "phone": "0212-1234567",
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
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices/structured", "tok-rest", mustJSON(t, payload), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	var out structuredInvoiceResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	want := []string{
		"iR*J-12345678-9",
		"iS*CLIENTE C.A.",
		"i01Av. 1",
		"i02" + "0212-1234567",
		"!   1.000     10.00CAFE",
		"3",
		"7001.00",
		"103tarjeta",
		"1009.00",
		"101",
	}
	if len(out.CompiledLines) != len(want) {
		t.Fatalf("líneas = %q", out.CompiledLines)
	}
	for i := range want {
		if out.CompiledLines[i] != want[i] {
			t.Fatalf("línea %d = %q, se esperaba %q", i, out.CompiledLines[i], want[i])
		}
	}
	if out.Totals.Total != 9.00 || out.Totals.Discount != 1.00 {
		t.Fatalf("totales = %+v", out.Totals)
	}
	if !out.Completed || out.InvoiceNumber != 1 {
		t.Fatalf("factura = %+v", out)
	}
	if got := fp.commandCount(); got != len(want) {
		t.Fatalf("comandos = %d, se esperaban %d", got, len(want))
	}
}

// El orden canónico de TFHKA manda los datos del cliente antes de los ítems:
// la API debe aceptarlos y el turno se toma desde la primera línea.
func TestHTTPInvoiceAcceptsCustomerLinesFirst(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})
	payload := mustJSON(t, map[string]any{
		"station": "ECOM-01",
		"lines":   []string{"iR*J-12345678-9", "iS*CLIENTE C.A.", "!   1.000     10.00CAFE", "101"},
	})
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
}

func TestHTTPOrderRefKeepsOriginalClientRef(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{})

	// Con client_ref: order_ref lo conserva y document lleva el sufijo de línea.
	payload := mustJSON(t, map[string]any{
		"station":    "ECOM-01",
		"client_ref": "pedido-1001",
		"lines":      []string{"!   1.000     10.00CAFE", "101"},
	})
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	var out struct {
		OrderRef string `json:"order_ref"`
		Document *struct {
			ClientRef string `json:"client_ref"`
		} `json:"document"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if out.OrderRef != "pedido-1001" {
		t.Fatalf("order_ref = %q", out.OrderRef)
	}
	if out.Document == nil || out.Document.ClientRef != "pedido-1001#1" {
		t.Fatalf("document.client_ref = %+v", out.Document)
	}

	// Sin client_ref: order_ref toma la clave de idempotencia.
	payload = mustJSON(t, map[string]any{
		"station": "ECOM-01",
		"lines":   []string{"!   1.000     10.00CAFE", "101"},
	})
	resp, body = httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload,
		map[string]string{"Idempotency-Key": "pedido-2002"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if raw["order_ref"] != "pedido-2002" {
		t.Fatalf("order_ref = %v", raw["order_ref"])
	}

	// Sin client_ref ni clave: el campo se omite.
	payload = mustJSON(t, map[string]any{
		"station": "ECOM-01",
		"lines":   []string{"!   1.000     10.00CAFE", "101"},
	})
	resp, body = httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	raw = nil
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if _, ok := raw["order_ref"]; ok {
		t.Fatalf("order_ref no debería aparecer: %v", raw["order_ref"])
	}
}
