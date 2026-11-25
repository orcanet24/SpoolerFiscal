package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"spoolerfiscal/internal/fiscal"
)

// cancelar un trabajo pausado (no impreso) lo deja cancelado, no se ejecuta
// cuando la impresora vuelve, y libera la clave de idempotencia para que el
// mismo pedido pueda re-emitirse.
func TestCancelPausedJobFreesIdempotency(t *testing.T) {
	srv, api, fp, sp, store := testJobsAPI(t, SpoolerConfig{
		Guard:                 GuardConfig{BlockOnPaperOut: true},
		AutoCloseOpenDocument: true,
	})
	fp.mu.Lock()
	fp.status = fiscal.Status{Online: true, PaperOut: true}
	fp.mu.Unlock()
	if _, err := sp.ProbeStatus(context.Background()); err != nil {
		t.Fatalf("ProbeStatus: %v", err)
	}

	payload := invoicePayload(t, "!   1.000     10.00CAFE", "101")
	hdr := map[string]string{"Idempotency-Key": "pedido-cancel-1"}
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, hdr)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("factura pausada: estado = %d (%s)", resp.StatusCode, body)
	}
	var eb invoiceErrBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("JSON de error: %v (%s)", err, body)
	}
	jobID := eb.Error.JobID
	if jobID == "" {
		t.Fatalf("sin job_id: %s", body)
	}

	resp, body = httpDo(t, srv, http.MethodPost, "/v1/jobs/"+jobID+"/cancel", "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancelar: estado = %d (%s)", resp.StatusCode, body)
	}
	var cancelled jobDetailJSON
	if err := json.Unmarshal(body, &cancelled); err != nil {
		t.Fatalf("JSON de cancelación: %v (%s)", err, body)
	}
	if cancelled.Status != JobCanceled {
		t.Fatalf("estado = %q", cancelled.Status)
	}

	// La impresora vuelve: el despachador no debe imprimir lo cancelado.
	fp.mu.Lock()
	fp.status = fiscal.Status{Online: true}
	fp.mu.Unlock()
	if _, err := sp.ProbeStatus(context.Background()); err != nil {
		t.Fatalf("ProbeStatus: %v", err)
	}
	api.dispatchTick(context.Background())

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobCanceled {
		t.Fatalf("el trabajo cancelado se ejecutó: %+v", job)
	}
	if got := fp.commandCount(); got != 0 {
		t.Fatalf("se enviaron %d comandos a la impresora tras cancelar", got)
	}

	// Re-emitir el mismo pedido con la misma clave ya no está bloqueado.
	resp, body = httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-emisión tras cancelar: estado = %d (%s)", resp.StatusCode, body)
	}
	var inv invoiceResponse
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("JSON de factura: %v (%s)", err, body)
	}
	if !inv.Completed || inv.InvoiceNumber != 1 {
		t.Fatalf("factura re-emitida = %+v", inv)
	}
}

// Un trabajo que ya se imprimió no se cancela: la factura existe en papel.
func TestCancelDoneJobIsRejected(t *testing.T) {
	srv, _, _, _, _ := testJobsAPI(t, SpoolerConfig{})

	payload := invoicePayload(t, "!   1.000     10.00CAFE", "101")
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("factura: estado = %d (%s)", resp.StatusCode, body)
	}
	var inv invoiceResponse
	if err := json.Unmarshal(body, &inv); err != nil || inv.JobID == "" {
		t.Fatalf("JSON de factura: %v (%s)", err, body)
	}

	resp, body = httpDo(t, srv, http.MethodPost, "/v1/jobs/"+inv.JobID+"/cancel", "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("cancelar trabajo impreso: estado = %d (%s)", resp.StatusCode, body)
	}
	var eb invoiceErrBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("JSON de error: %v (%s)", err, body)
	}
	if eb.Error.Code != "job_not_cancelable" {
		t.Fatalf("código = %q (%s)", eb.Error.Code, body)
	}
}

// Reintentar un trabajo pausado lo devuelve a la cola y el despachador lo
// completa cuando la impresora ya responde.
func TestRetryPausedJobRequeuesAndCompletes(t *testing.T) {
	srv, api, fp, sp, store := testJobsAPI(t, SpoolerConfig{
		Guard:                 GuardConfig{BlockOnPaperOut: true},
		AutoCloseOpenDocument: true,
	})
	fp.mu.Lock()
	fp.status = fiscal.Status{Online: true, PaperOut: true}
	fp.mu.Unlock()
	if _, err := sp.ProbeStatus(context.Background()); err != nil {
		t.Fatalf("ProbeStatus: %v", err)
	}

	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest",
		invoicePayload(t, "!   1.000     10.00CAFE", "101"), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("factura pausada: estado = %d (%s)", resp.StatusCode, body)
	}
	var eb invoiceErrBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("JSON de error: %v (%s)", err, body)
	}

	// Se repone el papel y el operador pide reintentar antes del siguiente tick.
	fp.mu.Lock()
	fp.status = fiscal.Status{Online: true}
	fp.mu.Unlock()
	if _, err := sp.ProbeStatus(context.Background()); err != nil {
		t.Fatalf("ProbeStatus: %v", err)
	}

	resp, body = httpDo(t, srv, http.MethodPost, "/v1/jobs/"+eb.Error.JobID+"/retry", "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("reintentar: estado = %d (%s)", resp.StatusCode, body)
	}
	var accepted jobAcceptedJSON
	if err := json.Unmarshal(body, &accepted); err != nil {
		t.Fatalf("JSON de reintento: %v (%s)", err, body)
	}
	if accepted.Status != JobQueued {
		t.Fatalf("estado = %q", accepted.Status)
	}

	api.dispatchTick(context.Background())

	job, err := store.GetJob(context.Background(), eb.Error.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobDone {
		t.Fatalf("el trabajo debía completarse; estado = %s (%q)", job.Status, job.LastError)
	}
	if got := fp.commandCount(); got != 2 {
		t.Fatalf("comandos a la impresora = %d (se esperaban ítem y cierre)", got)
	}
}

// Un trabajo con líneas ya impresas no se reintenta: se re-somete la factura.
func TestRetryPartialJobIsRejected(t *testing.T) {
	srv, _, _, _, store := testJobsAPI(t, SpoolerConfig{})
	ctx := context.Background()

	job := &PrintJob{
		ID: "job_parcial", Station: "WEB-01", Session: "api:WEB-01",
		Mode: "sync", Lines: []string{"!   1.000     10.00CAFE", "101"},
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := store.SaveJobCursor(ctx, job.ID, 1); err != nil {
		t.Fatalf("SaveJobCursor: %v", err)
	}
	if err := store.SetJobStatus(ctx, job.ID, JobPaused, "parcial", nil); err != nil {
		t.Fatalf("SetJobStatus: %v", err)
	}

	resp, body := httpDo(t, srv, http.MethodPost, "/v1/jobs/"+job.ID+"/retry", "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("reintentar parcial: estado = %d (%s)", resp.StatusCode, body)
	}
	var eb invoiceErrBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("JSON de error: %v (%s)", err, body)
	}
	if eb.Error.Code != "job_not_retryable" || !strings.Contains(eb.Error.Message, "1 de 2") {
		t.Fatalf("error = %+v (%s)", eb.Error, body)
	}
}

func TestCancelUnknownJobIsNotFound(t *testing.T) {
	srv, _, _, _, _ := testJobsAPI(t, SpoolerConfig{})
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/jobs/job_inexistente/cancel", "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cancelar inexistente: estado = %d (%s)", resp.StatusCode, body)
	}
}

// El compilador de /v1/invoices/structured emite TFHKA: con otra marca debe
// rechazar en vez de mandar líneas que la impresora no entiende.
func TestStructuredInvoiceRejectsNonTFHKADriver(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{Driver: fiscal.BrandHasar})
	body := mustJSON(t, map[string]any{
		"station": "ECOM-01",
		"items": []map[string]any{
			{"description": "CAFE", "quantity": 1, "unit_price": 10.0, "tax": "general"},
		},
	})
	resp, data := httpDo(t, srv, http.MethodPost, "/v1/invoices/structured", "tok-rest", body, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, data)
	}
	var eb invoiceErrBody
	if err := json.Unmarshal(data, &eb); err != nil {
		t.Fatalf("JSON de error: %v (%s)", err, data)
	}
	if eb.Error.Code != "structured_not_supported" {
		t.Fatalf("código = %q (%s)", eb.Error.Code, data)
	}
}

// La recuperación de documento colgado usa el cierre del driver, no el 101 de
// TFHKA a ciegas.
func TestSpoolerUsesDriverCloseCommand(t *testing.T) {
	fp := newFakePrinter(t)
	sp := NewSpooler(fp, nil, SpoolerConfig{}, testLogger())
	if got := string(sp.closeDocumentCommand()); got != "101" {
		t.Fatalf("driver sin DocumentCloser, cierre = %q", got)
	}

	hasar, err := fiscal.New(fiscal.BrandHasar, fiscal.Options{Logger: testLogger()})
	if err != nil {
		t.Fatalf("New(hasar): %v", err)
	}
	sp = NewSpooler(hasar, nil, SpoolerConfig{}, testLogger())
	if got := string(sp.closeDocumentCommand()); got != "@Close" {
		t.Fatalf("driver IxBatch, cierre = %q", got)
	}
}

func TestStoreCancelAndRetryTransitions(t *testing.T) {
	_, _, _, _, store := testJobsAPI(t, SpoolerConfig{})
	ctx := context.Background()

	job := &PrintJob{ID: "job_trans", Station: "W", Session: "api:W", Mode: "sync",
		Lines: []string{"!i", "101"}}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := store.CancelJob(ctx, "job_trans"); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if _, err := store.RetryJob(ctx, "job_trans"); err == nil {
		t.Fatal("un trabajo cancelado no debe reintentarse")
	}
	if _, err := store.CancelJob(ctx, "job_trans"); err == nil {
		t.Fatal("un trabajo cancelado no debe cancelarse dos veces")
	}

	// Reintentar exige estado pausado/encolado y avance cero.
	other := &PrintJob{ID: "job_otro", Station: "W", Session: "api:W", Mode: "sync",
		Lines: []string{"!i", "101"}}
	if err := store.CreateJob(ctx, other); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	retried, err := store.RetryJob(ctx, "job_otro")
	if err != nil {
		t.Fatalf("RetryJob: %v", err)
	}
	if retried.Status != JobQueued || retried.Mode != "async" {
		t.Fatalf("trabajo tras reintento = %+v", retried)
	}
}
