package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spoolerfiscal/internal/fiscal"
)

// testJobsAPI construye la API REST con acceso directo a la instancia para
// poder disparar el despachador (dispatchTick) de forma determinista.
func testJobsAPI(t *testing.T, sc SpoolerConfig) (*httptest.Server, *HTTPAPI, *fakePrinter, *Spooler, *Store) {
	t.Helper()
	fp := newFakePrinter(t)
	sp, store := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, sc)
	api := NewHTTPAPI(HTTPConfig{
		Token:             "tok-rest",
		Version:           "test",
		Driver:            "tfhka",
		MaxBodyBytes:      4096,
		DefaultTimeout:    5 * time.Second,
		JobResumeInterval: 50 * time.Millisecond,
	}, sp, store, testLogger())
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, api, fp, sp, store
}

// invoiceErrBody lee el sobre de error plano: code/message + detalles embebidos
// (job_id, line_index, document_open).
type invoiceErrBody struct {
	Error struct {
		Code         string `json:"code"`
		Message      string `json:"message"`
		JobID        string `json:"job_id"`
		LineIndex    *int   `json:"line_index"`
		DocumentOpen bool   `json:"document_open"`
	} `json:"error"`
}

func invoicePayload(t *testing.T, lines ...string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{"station": "WEB-01", "lines": lines})
}

// ---------- pausa y reanudación (papel agotado) ----------

func TestInvoiceJobPausedAndResumed(t *testing.T) {
	srv, api, fp, sp, store := testJobsAPI(t, SpoolerConfig{
		Guard:                 GuardConfig{BlockOnPaperOut: true},
		AutoCloseOpenDocument: true,
	})
	fp.mu.Lock()
	fp.status = fiscal.Status{Online: true, PaperOut: true}
	fp.mu.Unlock()
	// El guardián usa el estado cacheado: la sonda lo carga con papel agotado.
	if _, err := sp.ProbeStatus(context.Background()); err != nil {
		t.Fatalf("ProbeStatus: %v", err)
	}

	payload := invoicePayload(t, "!   1.000     10.00CAFE", "101")
	hdr := map[string]string{"Idempotency-Key": "pedido-2001"}

	// Papel agotado: la factura no llegó a papel → queda pausada en la BD.
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, hdr)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	var eb invoiceErrBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("JSON de error: %v (%s)", err, body)
	}
	if eb.Error.Code != "printer_blocked" || eb.Error.JobID == "" {
		t.Fatalf("sobre de pausa inesperado: %s", body)
	}
	job, err := store.GetJob(context.Background(), eb.Error.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobPaused || job.Cursor != 0 {
		t.Fatalf("trabajo = %+v", job)
	}

	// Se repone el papel y la sonda refresca el guardián.
	fp.mu.Lock()
	fp.status = fiscal.Status{Online: true}
	fp.mu.Unlock()
	if _, err := sp.ProbeStatus(context.Background()); err != nil {
		t.Fatalf("ProbeStatus: %v", err)
	}

	// El despachador reanuda el trabajo solo.
	api.dispatchTick(context.Background())

	job, err = store.GetJob(context.Background(), eb.Error.JobID)
	if err != nil {
		t.Fatalf("GetJob tras reanudar: %v", err)
	}
	if job.Status != JobDone {
		t.Fatalf("el trabajo debía completarse; estado = %s (last_error = %q)", job.Status, job.LastError)
	}

	// El reintento con la misma clave repara la respuesta: ahora es 201.
	resp, body = httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay estado = %d (%s)", resp.StatusCode, body)
	}
	var inv invoiceResponse
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("JSON de replay: %v", err)
	}
	if !inv.Completed || inv.InvoiceNumber != 1 {
		t.Fatalf("replay inesperado: %+v", inv)
	}
	if got := fp.commandCount(); got != 2 {
		t.Fatalf("el replay no debía reimprimir; comandos = %d", got)
	}
}

// ---------- aborto a mitad de factura con cierre de recuperación ----------

func TestInvoiceJobAbortedMidway(t *testing.T) {
	srv, _, fp, _, store := testJobsAPI(t, SpoolerConfig{AutoCloseOpenDocument: true})
	fp.mu.Lock()
	fp.failAfterN = 1 // el primer comando llega a papel, el segundo falla
	fp.mu.Unlock()

	payload := invoicePayload(t, "!   1.000     10.00CAFE", "!   1.000     20.00TE", "101")
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	var eb invoiceErrBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("JSON de error: %v (%s)", err, body)
	}
	if eb.Error.Code != "invoice_aborted" || eb.Error.JobID == "" {
		t.Fatalf("sobre de aborto inesperado: %s", body)
	}
	if eb.Error.LineIndex == nil || *eb.Error.LineIndex != 1 {
		t.Fatalf("line_index = %v", eb.Error.LineIndex)
	}

	job, err := store.GetJob(context.Background(), eb.Error.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobAborted || job.Cursor != 1 {
		t.Fatalf("trabajo = %+v", job)
	}

	// El spooler debe haber enviado el cierre 101 de recuperación para no
	// dejar la impresora con documento abierto (mejor: nunca entrelazar).
	fp.mu.Lock()
	var sawRecovery bool
	for _, c := range fp.commands {
		if strings.TrimSpace(string(c)) == "101" {
			sawRecovery = true
		}
	}
	docOpen := fp.openDoc
	fp.mu.Unlock()
	if !sawRecovery {
		t.Fatal("falta el cierre de recuperación (101) tras el aborto")
	}
	if docOpen {
		t.Fatal("la impresora quedó con documento abierto tras la recuperación")
	}

	// La siguiente factura fluye normal.
	resp, body = httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("factura siguiente estado = %d (%s)", resp.StatusCode, body)
	}
}

// ---------- modo asíncrono ----------

func TestInvoiceAsyncViaDispatcher(t *testing.T) {
	srv, api, _, _, store := testJobsAPI(t, SpoolerConfig{})

	payload := invoicePayload(t, "!   1.000     10.00CAFE", "101")
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices?async=1", "tok-rest", payload, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
	}
	var acc jobAcceptedJSON
	if err := json.Unmarshal(body, &acc); err != nil {
		t.Fatalf("JSON de aceptación: %v", err)
	}
	if acc.JobID == "" || acc.Status != JobQueued {
		t.Fatalf("aceptación = %+v", acc)
	}

	// Antes del tick el trabajo sigue en cola.
	job, err := store.GetJob(context.Background(), acc.JobID)
	if err != nil || job.Status != JobQueued {
		t.Fatalf("trabajo antes del tick: %+v (%v)", job, err)
	}

	api.dispatchTick(context.Background())

	job, err = store.GetJob(context.Background(), acc.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != JobDone {
		t.Fatalf("trabajo no completado: %+v", job)
	}
	var inv invoiceResponse
	if err := json.Unmarshal(job.Result, &inv); err != nil {
		t.Fatalf("resultado del trabajo: %v", err)
	}
	if !inv.Completed || inv.InvoiceNumber != 1 {
		t.Fatalf("resultado = %+v", inv)
	}

	// Listado y detalle REST.
	resp, body = httpDo(t, srv, http.MethodGet, "/v1/jobs?status=done", "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), acc.JobID) {
		t.Fatalf("listado = %d (%s)", resp.StatusCode, body)
	}
	resp, body = httpDo(t, srv, http.MethodGet, "/v1/jobs/"+acc.JobID, "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"done"`) {
		t.Fatalf("detalle = %d (%s)", resp.StatusCode, body)
	}
	resp, _ = httpDo(t, srv, http.MethodGet, "/v1/jobs/inexistente", "tok-rest", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("trabajo inexistente estado = %d", resp.StatusCode)
	}
}

// ---------- validación de secuencia ----------

func TestInvoiceSequenceValidation(t *testing.T) {
	srv, _, fp, _, _ := testJobsAPI(t, SpoolerConfig{})

	cases := []struct {
		name  string
		lines []string
		want  string // mensaje esperado (parcial)
	}{
		{"primera línea no abre", []string{"101"}, "la primera línea debe abrir"},
		{"ítem tras el cierre", []string{"!   1.000     10.00CAFE", "101", "!   1.000     5.00TE"}, "ítem después del cierre"},
		{"sin cierre final", []string{"!   1.000     10.00CAFE"}, "no se cierra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", invoicePayload(t, tc.lines...), nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("estado = %d (%s)", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Fatalf("mensaje inesperado: %s", body)
			}
		})
	}
	if got := fp.commandCount(); got != 0 {
		t.Fatalf("una factura inválida no debe llegar a la impresora; comandos = %d", got)
	}
}

// ---------- idempotencia con trabajo en curso ----------

func TestIdempotentRetryWhileJobActive(t *testing.T) {
	srv, _, fp, _, _ := testJobsAPI(t, SpoolerConfig{})
	fp.mu.Lock()
	fp.delay = 150 * time.Millisecond // la factura tarda ~300ms
	fp.mu.Unlock()

	payload := invoicePayload(t, "!   1.000     10.00CAFE", "101")
	hdr := map[string]string{"Idempotency-Key": "pedido-3001"}

	// Primera petición en vuelo.
	first := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/invoices", strings.NewReader(string(payload)))
		req.Header.Set("Authorization", "Bearer tok-rest")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "pedido-3001")
		resp, err := srv.Client().Do(req)
		if err != nil {
			first <- 0
			return
		}
		st := resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		first <- st
	}()

	// Espera a que el trabajo exista (202 provisional + fila en BD) pero
	// todavía esté ejecutándose: el registro ocurre antes de la impresión.
	time.Sleep(50 * time.Millisecond)

	// El reintento con la misma clave ve el trabajo activo: 202, sin duplicado.
	resp, body := httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", payload, hdr)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("reintento estado = %d (%s)", resp.StatusCode, body)
	}
	var acc jobAcceptedJSON
	if err := json.Unmarshal(body, &acc); err != nil || acc.JobID == "" {
		t.Fatalf("aceptación del reintento: %v (%s)", err, body)
	}

	if st := <-first; st != http.StatusCreated {
		t.Fatalf("petición original terminó en %d", st)
	}
	// El reintento no creó una segunda factura: sólo 2 comandos en total.
	time.Sleep(50 * time.Millisecond)
	if got := fp.commandCount(); got != 2 {
		t.Fatalf("comandos = %d (se duplicó la factura)", got)
	}

	// Conflicto de cuerpo con la misma clave.
	resp, body = httpDo(t, srv, http.MethodPost, "/v1/invoices", "tok-rest", invoicePayload(t, "101"), hdr)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicto estado = %d (%s)", resp.StatusCode, body)
	}
}

// ---------- guardián de documento abierto ----------

func TestRecoverOpenDocumentCuresAndUnblocks(t *testing.T) {
	_, _, fp, sp, _ := testJobsAPI(t, SpoolerConfig{AutoCloseOpenDocument: true})

	// La impresora quedó con documento abierto (p.ej. corte a mitad).
	fp.mu.Lock()
	fp.status = fiscal.Status{Online: true, DocumentOpen: true}
	fp.mu.Unlock()

	// La sonda carga el estado real al guardián (el guardián usa el estado
	// cacheado de la última consulta, como el driver real).
	if _, err := sp.ProbeStatus(context.Background()); err != nil {
		t.Fatalf("ProbeStatus: %v", err)
	}

	// El guardián bloquea nuevas emisiones mientras el colgador no se cure.
	_, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("!   1.000     10.00CAFE"), Station: "CAJA-01", Session: "s", Timeout: 5 * time.Second,
	})
	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "DOCUMENT_OPEN_RECOVERY" {
		t.Fatalf("se esperaba DOCUMENT_OPEN_RECOVERY, se obtuvo %v", err)
	}

	// La sesión que reclama como dueño legítimo no aplica: nadie lo reclama.
	ok, err := sp.RecoverOpenDocument(context.Background(), "")
	if err != nil {
		t.Fatalf("RecoverOpenDocument: %v", err)
	}
	if !ok {
		t.Fatal("la recuperación debía ejecutarse")
	}

	// El cierre 101 llegó a la impresora y el guardián se desbloquea.
	fp.mu.Lock()
	sawRecovery := len(fp.commands) > 0 && strings.TrimSpace(string(fp.commands[len(fp.commands)-1])) == "101"
	fp.mu.Unlock()
	if !sawRecovery {
		t.Fatal("falta el 101 de recuperación en la impresora")
	}
	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("!   1.000     10.00CAFE"), Station: "CAJA-01", Session: "s", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("tras la recuperación el guardián debía permitir emitir: %v", err)
	}
}

func TestRecoverOpenDocumentRespectsLiveOwner(t *testing.T) {
	fp := newFakePrinter(t)
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{
		AutoCloseOpenDocument: true,
	})

	// Un dueño vivo abrió su factura.
	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("!   1.000     10.00CAFE"), Station: "CAJA-01", Session: "s1", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("abrir factura: %v", err)
	}

	// Otro proceso pide recuperación: no debe tocar el turno del dueño vivo.
	ok, err := sp.RecoverOpenDocument(context.Background(), "otro")
	if err != nil {
		t.Fatalf("RecoverOpenDocument: %v", err)
	}
	if ok {
		t.Fatal("no debía cerrar el documento de una sesión viva")
	}
	if got := fp.commandCount(); got != 1 {
		t.Fatalf("comandos = %d (se envió 101 sobre un turno con dueño)", got)
	}

	// El dueño legítimo sí puede cerrar su propia factura colgada.
	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("101"), Station: "CAJA-01", Session: "s1", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("cierre del dueño: %v", err)
	}
}

// ---------- conciliación de correlativo (la impresora predomina) ----------

// syncPrinter expone la capacidad CorrelativeSource sobre el fake: así el
// spooler inyecta la consulta S1 antes de abrir cada documento. Registra las
// consultas para poder verificar la inyección del planificador.
type syncPrinter struct {
	*fakePrinter
	syncCalls int
}

func (s *syncPrinter) GetCorrelative(context.Context) (fiscal.Info, error) {
	s.fakePrinter.mu.Lock()
	defer s.fakePrinter.mu.Unlock()
	s.syncCalls++
	return fiscal.Info{
		Brand: "tfhka", Serial: "SIM-01", RIF: "J-1",
		LastInvoice: s.fakePrinter.docSeq,
	}, nil
}

func TestCorrelativeSyncInjectedBeforeOpenDocument(t *testing.T) {
	fp := newFakePrinter(t)
	wrapped := &syncPrinter{fakePrinter: fp}
	sp, store := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), wrapped, SpoolerConfig{})

	syncCalls := func() int {
		wrapped.fakePrinter.mu.Lock()
		defer wrapped.fakePrinter.mu.Unlock()
		return wrapped.syncCalls
	}

	// Antes de la primera factura el correlativo no está vivo todavía.
	if exp := sp.ExpectedCorrelative(fiscal.DocInvoice); exp != 1 {
		t.Fatalf("correlativo esperado inicial = %d", exp)
	}

	// La conciliación de arranque (TriggerCorrelativeSync) completa primero:
	// con la memoria vacía no fija número, pero marca el dato vivo.
	deadline := time.Now().Add(3 * time.Second)
	for syncCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if syncCalls() == 0 {
		t.Fatal("no corrió la conciliación de arranque")
	}

	// La impresora emite facturas fuera del spooler (p.ej. modo offline del
	// POS): el correlativo del sistema queda viejo.
	fp.mu.Lock()
	fp.docSeq = 5
	fp.mu.Unlock()

	// Se vence la edad del correlativo: el planificador debe inyectar la
	// conciliación S1 ANTES de ejecutar el ítem que abre el documento.
	sp.schedMu.Lock()
	sp.correlativeAt = time.Time{}
	sp.schedMu.Unlock()

	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("!   1.000     10.00CAFE"), Station: "CAJA-01", Session: "s1", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("ítem: %v", err)
	}

	// La conciliación corrió antes del ítem (el Submit sólo regresa cuando el
	// ítem ya se imprimió) y la impresora predominó: 5 → siguiente 6.
	if got := syncCalls(); got < 2 {
		t.Fatalf("conciliaciones = %d (faltó la inyección previa al ítem)", got)
	}
	if exp := sp.ExpectedCorrelative(fiscal.DocInvoice); exp != 6 {
		t.Fatalf("la impresora debía predominar: esperado = %d, se esperaba 6", exp)
	}

	// El cierre de la primera factura concilia con el número real impreso.
	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("101"), Station: "CAJA-01", Session: "s1", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("cierre: %v", err)
	}
	if exp := sp.ExpectedCorrelative(fiscal.DocInvoice); exp != 7 {
		t.Fatalf("tras la factura #6 el correlativo esperado = %d, se esperaba 7", exp)
	}

	// La BD registra el correlativo con fuente "printer".
	rels, err := store.Correlatives(context.Background())
	if err != nil {
		t.Fatalf("Correlatives: %v", err)
	}
	if rels[fiscal.DocInvoice] != 6 {
		t.Fatalf("correlativo persistido = %d", rels[fiscal.DocInvoice])
	}

	// Una conciliación forzada (la pre-vuelo de la API REST) también ajusta.
	fp.mu.Lock()
	fp.docSeq = 7
	fp.mu.Unlock()
	if _, err := sp.SyncCorrelative(context.Background(), true); err != nil {
		t.Fatalf("SyncCorrelative: %v", err)
	}
	if exp := sp.ExpectedCorrelative(fiscal.DocInvoice); exp != 8 {
		t.Fatalf("la impresora debía predominar tras conciliación forzada: esperado = %d, se esperaba 8", exp)
	}
}
