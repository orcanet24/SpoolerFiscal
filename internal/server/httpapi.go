package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/fiscal"
)

// ProtocolVersion es la versión del contrato de la API. El agente la valida
// contra GET /v1/health.
const ProtocolVersion uint32 = 1

// HTTPConfig configura la API REST.
type HTTPConfig struct {
	Token           string
	StationTokens   map[string]string
	AllowedStations []string
	Version         string
	Driver          string
	MaxBodyBytes    int64
	MaxLines        int
	MaxLineBytes    int
	DefaultTimeout  time.Duration
	MaxTimeout      time.Duration
	ProbeTimeout    time.Duration
	// JobResumeInterval es la frecuencia con que el despachador revisa la cola
	// duradera para reanudar trabajos pausados y ejecutar los asíncronos.
	// 0 = 10 segundos.
	JobResumeInterval time.Duration
	// JobResumeBatch limita cuántos trabajos reanuda por tick. 0 = 8.
	JobResumeBatch int
}

// HTTPAPI expone el spooler por HTTP/JSON: la usan los agentes de estación y los
// sistemas externos (ecommerce, ERP web, CRM). Usa el mismo Spooler que las
// estaciones: una sola cola, un solo turno por factura y un solo correlativo. Las facturas REST se
// persisten como trabajos (print_jobs): si la impresora se queda sin papel o
// se cae, la cola queda pausada en la BD y se reanuda sola cuando vuelve.
type HTTPAPI struct {
	cfg     HTTPConfig
	spooler *Spooler
	store   *Store
	log     *slog.Logger
	allowed map[string]struct{}

	// idemLocks serializa las peticiones que traen Idempotency-Key por clave
	// (no globalmente): dos reintentos simultáneos de la misma clave no emiten
	// dos facturas, y claves distintas no se bloquean entre sí.
	idemLocks   map[string]*keyMutex
	idemLocksMu sync.Mutex
}

type keyMutex struct {
	mu   sync.Mutex
	refs int
}

// NewHTTPAPI construye la API REST.
func NewHTTPAPI(cfg HTTPConfig, spooler *Spooler, store *Store, log *slog.Logger) *HTTPAPI {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 256 * 1024
	}
	if cfg.MaxLines <= 0 {
		cfg.MaxLines = 500
	}
	if cfg.MaxLineBytes <= 0 {
		cfg.MaxLineBytes = 1024
	}
	if cfg.JobResumeInterval <= 0 {
		cfg.JobResumeInterval = 10 * time.Second
	}
	if cfg.JobResumeBatch <= 0 {
		cfg.JobResumeBatch = 8
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedStations))
	for _, s := range cfg.AllowedStations {
		allowed[strings.TrimSpace(s)] = struct{}{}
	}
	return &HTTPAPI{cfg: cfg, spooler: spooler, store: store, log: log, allowed: allowed,
		idemLocks: map[string]*keyMutex{}}
}

// Handler devuelve el router HTTP de la API.
func (a *HTTPAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	// Documentación sin token: no contiene secretos y ayuda a integrar.
	mux.HandleFunc("GET /{$}", a.handleDocsRoot)
	mux.HandleFunc("GET /help", a.handleDocsRoot)
	mux.HandleFunc("GET /docs", a.handleDocs)
	mux.HandleFunc("GET /docs/{$}", a.handleDocs)
	mux.HandleFunc("GET /docs/manual.html", a.handleDocs)
	mux.HandleFunc("GET /docs/openapi.yaml", a.handleOpenAPI)
	mux.HandleFunc("GET /docs/diccionario", a.handleDiccionario)
	mux.HandleFunc("GET /docs/diccionario/{$}", a.handleDiccionario)
	mux.HandleFunc("GET /docs/diccionario/index.html", a.handleDiccionario)
	// Panel de operación: la página es pública (no contiene secretos), sus
	// llamadas exigen token.
	mux.HandleFunc("GET /admin", a.handleAdmin)
	mux.HandleFunc("GET /admin/{$}", a.handleAdmin)

	mux.HandleFunc("GET /v1/health", a.handleHealth)
	mux.HandleFunc("GET /v1/status", a.auth(a.handleStatus))
	mux.HandleFunc("GET /v1/printer", a.auth(a.handlePrinter))
	mux.HandleFunc("GET /v1/documents", a.auth(a.handleDocuments))
	mux.HandleFunc("POST /v1/commands", a.auth(a.handleCommand))
	mux.HandleFunc("POST /v1/invoices", a.auth(a.handleInvoice))
	mux.HandleFunc("POST /v1/invoices/structured", a.auth(a.handleStructuredInvoice))
	mux.HandleFunc("GET /v1/jobs", a.auth(a.handleJobs))
	mux.HandleFunc("GET /v1/jobs/{id}", a.auth(a.handleJob))
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", a.auth(a.handleCancelJob))
	mux.HandleFunc("POST /v1/jobs/{id}/retry", a.auth(a.handleRetryJob))
	return a.recoverer(a.logRequests(mux))
}

// ---------- endpoints ----------

func (a *HTTPAPI) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap := a.spooler.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"version":          a.cfg.Version,
		"protocol_version": ProtocolVersion,
		"driver":           a.cfg.Driver,
		"uptime_seconds":   int64(snap.Uptime.Seconds()),
		"server_time_unix": time.Now().Unix(),
	})
}

func (a *HTTPAPI) handleStatus(w http.ResponseWriter, r *http.Request) {
	if parseBool(r.URL.Query().Get("probe")) {
		if _, err := a.spooler.ProbeStatus(r.Context()); err != nil {
			a.log.Warn("sonda de estado HTTP falló", "error", err)
		}
	}
	snap := a.spooler.Snapshot()
	resp := statusJSON{
		Printer:            printerStatusJSONOf(snap.Status),
		Online:             snap.Status.Online,
		Blocked:            snap.Blocked != "",
		BlockedReason:      snap.Blocked,
		QueueDepth:         snap.QueueDepth,
		DocumentOpen:       snap.DocumentOpen,
		DocumentOwner:      snap.DocumentOwner,
		DocumentsIssued:    snap.Counters.Documents,
		CommandsTotal:      snap.Counters.Commands,
		CommandsFailed:     snap.Counters.Failures,
		DuplicatesDetected: snap.Counters.Duplicates,
		AvgLatencyMS:       snap.AvgLatency,
		MaxLatencyMS:       float64(snap.Counters.LatencyMaxMS),
		UptimeSeconds:      int64(snap.Uptime.Seconds()),
		LastSequence:       snap.LastSeq,
		HistoryCount:       snap.History.Count,
		LastInvoiceNumber:  snap.History.LastNumbers[fiscal.DocInvoice],
	}
	if d := snap.History.LastDocument; d != nil {
		resp.LastDocument = documentJSONOf(d)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *HTTPAPI) handlePrinter(w http.ResponseWriter, r *http.Request) {
	info, err := a.spooler.ProbeInfo(r.Context())
	if err != nil && info.Serial == "" {
		writeError(w, http.StatusServiceUnavailable, "printer_unavailable",
			"no se pudo consultar la impresora: "+err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"brand": info.Brand, "model": info.Model, "serial": info.Serial,
		"firmware": info.Firmware, "fiscal_rif": info.RIF, "port": info.Port,
		"fiscal_memory_total_mb": info.MemoryTotalMB,
		"fiscal_memory_free_mb":  info.MemoryFreeMB,
		"last_invoice_number":    info.LastInvoice,
		"last_credit_note":       info.LastCredit,
		"last_debit_note":        info.LastDebit,
		"last_non_fiscal_doc":    info.LastNonFiscal,
		"last_z_number":          info.LastZ,
	})
}

func (a *HTTPAPI) handleDocuments(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit debe ser un entero > 0", nil)
			return
		}
		limit = min(n, 500)
	}
	docs, err := a.store.Documents(r.Context(), limit)
	if err != nil {
		a.log.Error("listar documentos", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "no se pudo leer el historial", nil)
		return
	}
	out := make([]documentJSON, 0, len(docs))
	for i := range docs {
		out = append(out, *documentJSONOf(&docs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": out, "count": len(out)})
}

func (a *HTTPAPI) handleCommand(w http.ResponseWriter, r *http.Request) {
	var req commandRequest
	body, ok := a.decode(w, r, &req)
	if !ok {
		return
	}
	hash := a.requestHash(r, body)
	a.idempotent(w, r, func(hash string, async bool) (int, any) {
		return a.runCommand(r.Context(), req, r.Header.Get("Idempotency-Key"))
	}, hash, false)
}

func (a *HTTPAPI) handleInvoice(w http.ResponseWriter, r *http.Request) {
	var req invoiceRequest
	body, ok := a.decode(w, r, &req)
	if !ok {
		return
	}
	hash := a.requestHash(r, body)
	a.idempotent(w, r, func(hash string, async bool) (int, any) {
		return a.runInvoice(r.Context(), req, r.Header.Get("Idempotency-Key"), hash, async)
	}, hash, a.asyncRequested(r))
}

func (a *HTTPAPI) handleStructuredInvoice(w http.ResponseWriter, r *http.Request) {
	var req structuredInvoiceRequest
	body, ok := a.decode(w, r, &req)
	if !ok {
		return
	}
	hash := a.requestHash(r, body)
	a.idempotent(w, r, func(hash string, async bool) (int, any) {
		return a.runStructuredInvoice(r.Context(), req, r.Header.Get("Idempotency-Key"), hash, async)
	}, hash, a.asyncRequested(r))
}

// requestHash firma el cuerpo (método + ruta + bytes) para la idempotencia.
func (a *HTTPAPI) requestHash(r *http.Request, body []byte) string {
	sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
	return hex.EncodeToString(sum[:])
}

// asyncRequested detecta el modo asíncrono: ?async=1 o encabezado X-Async.
func (a *HTTPAPI) asyncRequested(r *http.Request) bool {
	if parseBool(r.URL.Query().Get("async")) {
		return true
	}
	return parseBool(r.Header.Get("X-Async"))
}

// ---------- trabajos duraderos (colas REST) ----------

func (a *HTTPAPI) handleJobs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit debe ser un entero > 0", nil)
			return
		}
		limit = min(n, 200)
	}
	jobs, err := a.store.ListJobs(r.Context(), strings.TrimSpace(r.URL.Query().Get("status")), limit)
	if err != nil {
		a.log.Error("listar trabajos", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "no se pudo leer la cola de trabajos", nil)
		return
	}
	out := make([]jobSummaryJSON, 0, len(jobs))
	for i := range jobs {
		out = append(out, jobSummaryJSONOf(&jobs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "count": len(out)})
}

func (a *HTTPAPI) handleJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.store.GetJob(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrJobNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "trabajo no encontrado", nil)
		return
	}
	if err != nil {
		a.log.Error("leer trabajo", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "no se pudo leer el trabajo", nil)
		return
	}
	resp := jobDetailJSONOf(&job)
	if len(job.Result) > 0 {
		resp.Result = json.RawMessage(job.Result)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCancelJob descarta un trabajo que todavía no llegó a papel (encolado o
// pausado, avance cero). Es la única cancelación segura: una factura que ya
// empezó a imprimirse no se puede "desimprimir".
func (a *HTTPAPI) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := a.store.CancelJob(r.Context(), id)
	switch {
	case errors.Is(err, ErrJobNotFound):
		writeError(w, http.StatusNotFound, "not_found", "trabajo no encontrado", nil)
		return
	case errors.Is(err, ErrJobNotCancelable):
		writeError(w, http.StatusConflict, "job_not_cancelable",
			fmt.Sprintf("el trabajo está en estado %q: una factura que ya empezó a imprimirse no se cancela, requiere revisión del operador", job.Status),
			&errorDetails{JobID: job.ID})
		return
	case err != nil:
		a.log.Error("cancelar trabajo", "job", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "no se pudo cancelar el trabajo", nil)
		return
	}
	// Cancelar sólo es posible sin líneas impresas, así que liberar la clave de
	// idempotencia es seguro: el mismo pedido puede re-emitirse sin quedar
	// bloqueado por el 202 de un trabajo que nunca llegó a papel.
	if job.IdemKey != "" {
		if derr := a.store.IdempotencyDelete(job.IdemKey); derr != nil {
			a.log.Warn("liberar idempotencia al cancelar", "job", job.ID, "error", derr)
		}
	}
	a.log.Info("trabajo cancelado por el operador", "job", job.ID, "estacion", job.Station)
	writeJSON(w, http.StatusOK, jobDetailJSONOf(&job))
}

// handleRetryJob devuelve a la cola un trabajo pausado o encolado que no
// imprimió nada. Un trabajo con avance no se reintenta: la factura quedaría
// partida en dos documentos.
func (a *HTTPAPI) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := a.store.RetryJob(r.Context(), id)
	switch {
	case errors.Is(err, ErrJobNotFound):
		writeError(w, http.StatusNotFound, "not_found", "trabajo no encontrado", nil)
		return
	case errors.Is(err, ErrJobNotRetryable):
		msg := fmt.Sprintf("el trabajo está en estado %q y no se puede reintentar", job.Status)
		if job.Cursor > 0 {
			msg = fmt.Sprintf("el trabajo imprimió %d de %d líneas: no se reimprime a mitad de factura; re-someta la factura completa",
				job.Cursor, len(job.Lines))
		}
		writeError(w, http.StatusConflict, "job_not_retryable", msg, &errorDetails{JobID: job.ID})
		return
	case err != nil:
		a.log.Error("reintentar trabajo", "job", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "no se pudo reintentar el trabajo", nil)
		return
	}
	a.log.Info("trabajo reencolado por el operador", "job", job.ID, "estacion", job.Station)
	writeJSON(w, http.StatusAccepted, jobAcceptedJSON{
		JobID: job.ID, Status: job.Status, Station: job.Station, Lines: len(job.Lines),
	})
}

type jobSummaryJSON struct {
	JobID     string    `json:"job_id"`
	Station   string    `json:"station"`
	Status    string    `json:"status"`
	Cursor    int       `json:"cursor"`
	Lines     int       `json:"lines"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func jobSummaryJSONOf(j *PrintJob) jobSummaryJSON {
	return jobSummaryJSON{
		JobID: j.ID, Station: j.Station, Status: j.Status,
		Cursor: j.Cursor, Lines: len(j.Lines), Attempts: j.Attempts,
		LastError: j.LastError, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
	}
}

type jobDetailJSON struct {
	jobSummaryJSON
	ClientRef string          `json:"client_ref,omitempty"`
	LinesRaw  []string        `json:"lines_raw,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

func jobDetailJSONOf(j *PrintJob) jobDetailJSON {
	return jobDetailJSON{
		jobSummaryJSON: jobSummaryJSONOf(j),
		ClientRef:      j.ClientRef,
		LinesRaw:       j.Lines,
	}
}

// ---------- ejecución ----------

// jobOutcome es el resultado de ejecutar (o intentar ejecutar) un trabajo.
type jobOutcome struct {
	Status    string // JobDone | JobPaused | JobAborted | JobPrinting
	Payload   any    // invoiceResponse cuando está done
	Err       error
	LineIndex int // -1 = sin línea asociada
}

// errJobInExecution indica que otro ejecutor tomó el trabajo.
var errJobInExecution = errors.New("factura en ejecución por otro proceso; consulte /v1/jobs/{id}")

// jobAcceptedJSON es la respuesta 202 del modo asíncrono y de los reintentos.
type jobAcceptedJSON struct {
	JobID   string `json:"job_id"`
	Status  string `json:"status"`
	Station string `json:"station,omitempty"`
	Lines   int    `json:"lines,omitempty"`
}

// spoolerError clasifica un error del spooler en estado HTTP y código.
func (a *HTTPAPI) spoolerError(err error) (int, string) {
	var blocked *BlockedError
	switch {
	case errors.Is(err, ErrQueueFull):
		return http.StatusTooManyRequests, "queue_full"
	case errors.As(err, &blocked):
		return http.StatusConflict, "printer_blocked"
	case errors.Is(err, ErrPrinterUnavailable), errors.Is(err, ErrPrinterTimeout):
		return http.StatusServiceUnavailable, "printer_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout"
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout, "canceled"
	case errors.Is(err, ErrClosed):
		return http.StatusServiceUnavailable, "shutting_down"
	default:
		a.log.Error("error del spooler en REST", "error", err)
		return http.StatusBadGateway, "printer_error"
	}
}

// responseText devuelve la respuesta como texto sólo si es UTF-8 válido. El
// agente usa response_base64, que preserva los bytes fiscales íntegros.
// Para el consumo humano (curl, paneles) se descarta la envoltura STX/ETX/LRC
// de TFHKA: los bytes exactos siguen disponibles en response_base64.
func responseText(b []byte) string {
	b = fiscal.UnframeSTX(b)
	if utf8.Valid(b) {
		return string(b)
	}
	return ""
}

// runCommand recibe un comando fiscal crudo y devuelve la respuesta de la
// impresora. Es el extremo que usa el agente de estación (COM virtual).
func (a *HTTPAPI) runCommand(ctx context.Context, req commandRequest, idemKey string) (int, any) {
	if status, msg := a.validateStation(ctx, req.Station); status != 0 {
		return status, errorEnvelopeOf(statusCode(status), msg, nil)
	}
	cmd := []byte(req.Command)
	if req.CommandBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.CommandBase64)
		if err != nil {
			return http.StatusBadRequest, errorEnvelopeOf("invalid_request", "command_base64 no es base64 válido", nil)
		}
		cmd = decoded
	}
	if len(cmd) == 0 {
		return http.StatusBadRequest, errorEnvelopeOf("invalid_request", "command o command_base64 es obligatorio", nil)
	}
	if len(cmd) > a.cfg.MaxLineBytes*64 {
		return http.StatusBadRequest, errorEnvelopeOf("invalid_request", "comando demasiado largo", nil)
	}

	session := "api:" + req.Station
	ref := req.ClientRef
	if ref == "" {
		ref = idemKey
	}
	res, err := a.spooler.Submit(ctx, SubmitRequest{
		Command:   cmd,
		Station:   req.Station,
		Session:   session,
		ClientRef: ref,
		Timeout:   a.timeout(req.TimeoutMS),
	})
	if err != nil {
		status, code := a.spoolerError(err)
		return status, errorEnvelopeOf(code, err.Error(), nil)
	}
	out := commandJSON{
		Station:        req.Station,
		Session:        session,
		Response:       responseText(res.Response),
		ResponseBase64: base64.StdEncoding.EncodeToString(res.Response),
		Sequence:       res.Sequence,
		QueueWaitMS:    res.QueueWait.Milliseconds(),
		LatencyMS:      res.Latency.Milliseconds(),
		Status:         printerStatusJSONOf(res.Status),
	}
	if res.Document != nil {
		out.Document = documentJSONOf(res.Document)
	}
	return http.StatusOK, out
}

// runStructuredInvoice compila ítems a líneas TFHKA, valida los totales y
// ejecuta la factura con la misma ruta atómica que /v1/invoices.
func (a *HTTPAPI) runStructuredInvoice(ctx context.Context, req structuredInvoiceRequest, idemKey, requestHash string, async bool) (int, any) {
	if status, msg := a.validateStation(ctx, req.Station); status != 0 {
		return status, errorEnvelopeOf(statusCode(status), msg, nil)
	}
	// El compilador de ítems conoce las tasas y el formato de TFHKA. Con otra
	// marca la vía correcta es /v1/invoices con las líneas del protocolo
	// nativo: inventar una secuencia IxBatch sin validar contra el hardware
	// sería un error fiscal, no una comodidad.
	if !strings.EqualFold(a.cfg.Driver, fiscal.BrandTFHKA) {
		return http.StatusNotImplemented, errorEnvelopeOf("structured_not_supported",
			fmt.Sprintf("el compilador structured emite protocolo TFHKA y el driver activo es %q; "+
				"envíe las líneas nativas por /v1/invoices (@OpenFiscalReceipt, @PrintLineItem, @Close)", a.cfg.Driver), nil)
	}
	lines, totals, err := compileInvoice(req, a.cfg.MaxLines)
	if err != nil {
		return http.StatusBadRequest, errorEnvelopeOf("invalid_request", err.Error(), nil)
	}
	if req.Totals != nil {
		if err := checkTotals(*req.Totals, totals); err != nil {
			return http.StatusBadRequest, errorEnvelopeOf("totals_mismatch", err.Error(), nil)
		}
	}

	status, payload := a.runInvoice(ctx, invoiceRequest{
		Station: req.Station, Lines: lines, ClientRef: req.ClientRef, TimeoutMS: req.TimeoutMS,
	}, idemKey, requestHash, async)
	if status != http.StatusCreated {
		return status, payload
	}
	inv, ok := payload.(invoiceResponse)
	if !ok {
		return status, payload
	}
	return status, structuredInvoiceResponse{invoiceResponse: inv, CompiledLines: lines, Totals: totals}
}

// validateSequence recorre las líneas con un mini-autómata: abre, ítems,
// cierre único al final. Rechaza en 400 lo que la impresora rechazaría a mitad
// de factura (mucho peor de depurar y deja documento colgado).
func validateSequence(step func([]byte) fiscal.Step, lines []string) (int, string) {
	const (
		stateNone = iota
		stateOpen
		stateClosed
	)
	state := stateNone
	for i, line := range lines {
		switch step([]byte(line)) {
		case fiscal.StepOpenDocument:
			if state == stateClosed {
				return i, fmt.Sprintf("línea %d: ítem después del cierre de la factura", i)
			}
			state = stateOpen
		case fiscal.StepCloseDocument:
			if state != stateOpen {
				return i, fmt.Sprintf("línea %d: cierre sin documento abierto", i)
			}
			state = stateClosed
		default:
			if state == stateClosed {
				return i, fmt.Sprintf("línea %d: comando después del cierre de la factura", i)
			}
		}
	}
	if state != stateClosed {
		return len(lines) - 1, "la factura no se cierra (falta el comando de cierre: 101 en TFHKA, @Close en IxBatch)"
	}
	return -1, ""
}

func (a *HTTPAPI) runInvoice(ctx context.Context, req invoiceRequest, idemKey, requestHash string, async bool) (int, any) {
	mode := "sync"
	if async {
		mode = "async"
	}
	if status, msg := a.validateStation(ctx, req.Station); status != 0 {
		return status, errorEnvelopeOf(statusCode(status), msg, nil)
	}
	if len(req.Lines) == 0 {
		return http.StatusBadRequest, errorEnvelopeOf("invalid_request", "lines es obligatorio (una o más líneas fiscales)", nil)
	}
	if len(req.Lines) > a.cfg.MaxLines {
		return http.StatusBadRequest, errorEnvelopeOf("invalid_request",
			fmt.Sprintf("demasiadas líneas (máx. %d)", a.cfg.MaxLines), nil)
	}
	for i, line := range req.Lines {
		if len(line) == 0 || len(line) > a.cfg.MaxLineBytes {
			return http.StatusBadRequest, errorEnvelopeOf("invalid_request",
				fmt.Sprintf("línea %d vacía o mayor de %d bytes", i, a.cfg.MaxLineBytes), nil)
		}
	}
	// Validación estructural completa: la primera línea abre, los ítems siguen
	// el documento y el cierre es único y final.
	if a.spooler.Step([]byte(req.Lines[0])) != fiscal.StepOpenDocument {
		return http.StatusBadRequest, errorEnvelopeOf("invalid_request",
			"la primera línea debe abrir el documento (ítem TFHKA, datos de cliente iR*/iS*, o @OpenFiscalReceipt en IxBatch)", nil)
	}
	if idx, msg := validateSequence(a.spooler.Step, req.Lines); idx >= 0 {
		return http.StatusBadRequest, errorEnvelopeOf("invalid_request", msg, &errorDetails{LineIndex: &idx})
	}

	session := "api:" + req.Station
	ref := req.ClientRef
	if ref == "" {
		ref = idemKey
	}

	// Pre-vuelo de correlativo: la impresora es la fuente legal. Antes de
	// enviar la primera línea se concilia S1; si no se puede consultar, no se
	// imprime (una factura con número equivocado no tiene arreglo posterior).
	if _, err := a.spooler.SyncCorrelative(ctx, false); err != nil {
		a.log.Warn("pre-vuelo de correlativo falló", "estacion", req.Station, "error", err)
		return http.StatusServiceUnavailable, errorEnvelopeOf("printer_unavailable",
			"no se pudo conciliar el correlativo con la impresora: "+err.Error(), nil)
	}
	expectedInvoice := a.spooler.ExpectedCorrelative(fiscal.DocInvoice)

	// La factura se persiste como trabajo: si la impresora falla a mitad, la
	// cola queda registrada en la BD y se decide pausar o abortar con evidencia.
	job := &PrintJob{
		ID: newJobID(), Station: req.Station, Session: session,
		IdemKey: idemKey, RequestHash: requestHash, ClientRef: ref,
		Mode: mode, Lines: append([]string(nil), req.Lines...),
	}
	if err := a.store.CreateJob(context.Background(), job); err != nil {
		a.log.Error("persistir trabajo de impresión", "error", err)
		return http.StatusInternalServerError, errorEnvelopeOf("internal_error", "no se pudo registrar la factura en la cola", nil)
	}
	a.log.Info("factura encolada", "estacion", req.Station, "job", job.ID,
		"lineas", len(job.Lines), "modo", job.Mode, "idempotencia", idemKey != "",
		"correlativo_esperado", expectedInvoice)

	// Marca provisional de idempotencia: un reintento inmediato ve el trabajo
	// activo (202) en lugar de crear una segunda factura.
	if idemKey != "" {
		acc, _ := json.Marshal(jobAcceptedJSON{JobID: job.ID, Status: JobQueued, Station: req.Station, Lines: len(job.Lines)})
		if err := a.store.IdempotencyPut(idemKey, requestHash, http.StatusAccepted, acc); err != nil {
			a.log.Warn("registrar idempotencia provisional", "error", err)
		}
	}

	if mode == "async" {
		return http.StatusAccepted, jobAcceptedJSON{JobID: job.ID, Status: job.Status, Station: req.Station, Lines: len(job.Lines)}
	}

	outcome := a.executeJob(ctx, job)
	return a.outcomeResponse(outcome, job.ID)
}

// executeJob toma el trabajo (atomic claim) y lo ejecuta de su cursor en
// adelante. Lo usan la petición sincrónica y el despachador de reanudación.
func (a *HTTPAPI) executeJob(ctx context.Context, job *PrintJob) jobOutcome {
	claimed, err := a.store.ClaimJob(context.Background(), job.ID)
	if err != nil {
		return jobOutcome{Status: job.Status, Err: err}
	}
	if !claimed {
		// El despachador u otro ejecutor lo tomó: el llamador debe consultar
		// /v1/jobs/{id}; la idempotencia evita duplicados.
		return jobOutcome{Status: JobPrinting, Err: errJobInExecution}
	}
	return a.runJobLines(ctx, job)
}

// runJobLines ejecuta las líneas pendientes y persiste el avance línea a línea.
func (a *HTTPAPI) runJobLines(ctx context.Context, job *PrintJob) jobOutcome {
	timeout := a.cfg.DefaultTimeout
	out := invoiceResponse{
		Station: job.Station, Session: job.Session, OrderRef: job.ClientRef,
		Completed: true, Lines: make([]lineJSON, 0, len(job.Lines)),
		JobID: job.ID,
	}
	for i := job.Cursor; i < len(job.Lines); i++ {
		lineRef := job.ClientRef
		if lineRef != "" {
			lineRef = fmt.Sprintf("%s#%d", lineRef, i)
		}
		res, err := a.spooler.Submit(ctx, SubmitRequest{
			Command:   []byte(job.Lines[i]),
			Station:   job.Station,
			Session:   job.Session,
			ClientRef: lineRef,
			Timeout:   timeout,
		})
		if err != nil {
			a.log.Warn("factura falló a mitad", "estacion", job.Station, "job", job.ID,
				"linea", i, "avance", job.Cursor, "error", err)
			return a.jobFailure(ctx, job, i, err, out)
		}
		out.Lines = append(out.Lines, lineJSON{
			Index: i, Response: responseText(res.Response), Sequence: res.Sequence,
			QueueWaitMS: res.QueueWait.Milliseconds(), LatencyMS: res.Latency.Milliseconds(),
		})
		if res.Document != nil {
			out.Document = documentJSONOf(res.Document)
			out.InvoiceNumber = res.Document.Number
			out.DocumentType = res.Document.Type
			out.Sequence = res.Document.Sequence
			out.IssuedAt = res.Document.IssuedAt.UTC().Format(time.RFC3339)
			out.Duplicate = res.Document.Duplicate
		}
		job.Cursor = i + 1
		if err := a.store.SaveJobCursor(context.Background(), job.ID, job.Cursor); err != nil {
			a.log.Error("persistir avance del trabajo", "job", job.ID, "cursor", job.Cursor, "error", err)
		}
	}
	out.ElapsedMS = 0
	encoded, err := json.Marshal(out)
	if err != nil {
		_ = a.store.SetJobStatus(context.Background(), job.ID, JobDone, "respuesta no serializable", nil)
		return jobOutcome{Status: JobDone, Payload: out}
	}
	if err := a.store.SetJobStatus(context.Background(), job.ID, JobDone, "", encoded); err != nil {
		a.log.Error("marcar trabajo completado", "job", job.ID, "error", err)
	}
	if job.IdemKey != "" {
		if err := a.store.IdempotencyReplace(job.IdemKey, http.StatusCreated, encoded); err != nil {
			a.log.Warn("finalizar idempotencia", "job", job.ID, "error", err)
		}
	}
	a.log.Info("factura REST emitida", "estacion", job.Station, "job", job.ID,
		"factura", out.InvoiceNumber, "lineas", len(out.Lines), "idempotencia", job.IdemKey != "")
	return jobOutcome{Status: JobDone, Payload: out}
}

// transientJobError clasifica los errores que valen la pena esperar: papel,
// impresora caída, cola llena, corte del cliente o del servidor. Si la factura
// no llegó a papel, el trabajo se pausa y se reanuda solo.
func transientJobError(err error) (reason string, ok bool) {
	switch {
	case errors.Is(err, ErrQueueFull),
		errors.Is(err, ErrPrinterUnavailable),
		errors.Is(err, ErrPrinterTimeout),
		errors.Is(err, ErrClosed),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		return "transitorio", true
	}
	var blocked *BlockedError
	if errors.As(err, &blocked) {
		return blocked.Reason, true
	}
	return "", false
}

// jobFailure decide pausar o abortar. Pausar sólo si ninguna línea llegó a
// papel y el impedimento es transitorio. Con avance (o error definitivo) se
// aborta: se intenta el cierre de recuperación del driver y el integrador re-somete.
func (a *HTTPAPI) jobFailure(ctx context.Context, job *PrintJob, lineIdx int, err error, out invoiceResponse) jobOutcome {
	if _, transient := transientJobError(err); transient && job.Cursor == 0 {
		if serr := a.store.SetJobStatus(context.Background(), job.ID, JobPaused, err.Error(), nil); serr != nil {
			a.log.Error("pausar trabajo", "job", job.ID, "error", serr)
		}
		a.log.Warn("factura pausada antes de imprimir: se reanudará sola", "job", job.ID, "error", err)
		return jobOutcome{Status: JobPaused, Err: err, Payload: out, LineIndex: lineIdx}
	}

	if job.Cursor > 0 {
		a.recoverAfterAbort(ctx, job, job.Session)
	}
	lastErr := err.Error()
	if job.Cursor > 0 {
		lastErr = fmt.Sprintf("parcial: %d de %d líneas impresas; %v (re-someta la factura completa)", job.Cursor, len(job.Lines), err)
	}
	_ = a.store.SetJobStatus(context.Background(), job.ID, JobAborted, lastErr, nil)
	if job.IdemKey != "" {
		body, _ := json.Marshal(errorEnvelopeOf("invoice_aborted", lastErr, &errorDetails{JobID: job.ID}))
		_ = a.store.IdempotencyReplace(job.IdemKey, http.StatusBadGateway, body)
	}
	return jobOutcome{Status: JobAborted, Err: err, Payload: out, LineIndex: lineIdx}
}

// recoverAfterAbort intenta cerrar el documento fiscal que la factura abortada
// dejó abierto. session es la sesión que tuvo el turno legítimo; si la
// impresora aún lo registra como su dueño, la recuperación puede curarlo.
// Mejor esfuerzo: si la impresora no responde, el guardián
// DOCUMENT_OPEN_RECOVERY bloqueará nuevas emisiones hasta lograrlo.
func (a *HTTPAPI) recoverAfterAbort(ctx context.Context, job *PrintJob, session string) {
	if ctx.Err() != nil {
		ctx = context.Background()
	}
	recoverCtx, cancel := context.WithTimeout(ctx, a.cfg.DefaultTimeout)
	defer cancel()
	if _, err := a.spooler.RecoverOpenDocument(recoverCtx, session); err != nil {
		a.log.Warn("cierre de recuperación tras aborto", "job", job.ID, "error", err)
	}
}

// outcomeResponse traduce el resultado del trabajo a la respuesta HTTP.
func (a *HTTPAPI) outcomeResponse(o jobOutcome, jobID string) (int, any) {
	switch o.Status {
	case JobDone:
		payload, _ := o.Payload.(invoiceResponse)
		payload.Completed = true
		return http.StatusCreated, payload
	case JobPaused:
		status, code := a.spoolerError(o.Err)
		details := &errorDetails{JobID: jobID}
		if o.LineIndex >= 0 {
			details.LineIndex = &o.LineIndex
		}
		return status, errorEnvelopeOf(code,
			"factura pausada en la cola: "+o.Err.Error(), details)
	case JobPrinting:
		return http.StatusAccepted, jobAcceptedJSON{JobID: jobID, Status: o.Status}
	default:
		details := &errorDetails{JobID: jobID}
		if o.LineIndex >= 0 {
			details.LineIndex = &o.LineIndex
		}
		return http.StatusBadGateway, errorEnvelopeOf("invoice_aborted",
			"factura abortada: "+o.Err.Error(), details)
	}
}

// Dispatcher es el bucle que mantiene sana la cola duradera: reanuda trabajos
// pausados cuando la impresora vuelve (papel, offline), ejecuta los trabajos
// asíncronos y cura documentos colgados con el cierre de recuperación.
func (a *HTTPAPI) Dispatcher(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.JobResumeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.dispatchTick(ctx)
		}
	}
}

func (a *HTTPAPI) dispatchTick(ctx context.Context) {
	jobs, err := a.store.ResumableJobs(ctx, a.cfg.JobResumeBatch)
	if err != nil {
		a.log.Error("leer cola duradera", "error", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	snap := a.spooler.Snapshot()
	if snap.Blocked != "" {
		if snap.Blocked == "DOCUMENT_OPEN_RECOVERY" {
			if _, rerr := a.spooler.RecoverOpenDocument(ctx, ""); rerr != nil {
				a.log.Warn("recuperación de documento abierto", "error", rerr)
			}
		}
		return // bloqueado (papel, memoria, error): los trabajos siguen pausados
	}
	// Si el estado está rancio u offline, sonda antes de imprimir: sin
	// respuesta no hay manera segura de saber si la impresora está lista.
	if !snap.Status.Online || time.Since(snap.Status.Checked) > 30*time.Second {
		if _, perr := a.spooler.ProbeStatus(ctx); perr != nil {
			a.log.Warn("sonda del despachador falló", "error", perr)
			return
		}
	}
	for i := range jobs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		j := jobs[i]
		claimed, cerr := a.store.ClaimJob(context.Background(), j.ID)
		if cerr != nil || !claimed {
			continue
		}
		a.log.Info("reanudando trabajo de impresión", "job", j.ID, "estacion", j.Station,
			"lineas", len(j.Lines), "avance", j.Cursor)
		out := a.runJobLines(ctx, &j)
		switch out.Status {
		case JobDone:
			a.log.Info("trabajo completado", "job", j.ID)
		case JobPaused:
			a.log.Warn("trabajo re-pausado", "job", j.ID, "error", out.Err)
		default:
			a.log.Error("trabajo abortado en reanudación", "job", j.ID, "error", out.Err)
		}
	}
}

func newJobID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("j%d", time.Now().UnixNano())
	}
	return "job_" + hex.EncodeToString(buf)
}

// ---------- idempotencia ----------

// lockIdemKey toma el lock exclusivo de una clave (sólo esa clave): dos
// reintentos simultáneos de la misma factura se serializan y otras claves
// siguen operando en paralelo. Devuelve la función de liberación.
func (a *HTTPAPI) lockIdemKey(key string) (unlock func()) {
	if key == "" {
		return func() {}
	}
	a.idemLocksMu.Lock()
	lm, ok := a.idemLocks[key]
	if !ok {
		lm = &keyMutex{}
		a.idemLocks[key] = lm
	}
	lm.refs++
	a.idemLocksMu.Unlock()
	lm.mu.Lock()
	return func() {
		lm.mu.Unlock()
		a.idemLocksMu.Lock()
		lm.refs--
		if lm.refs == 0 {
			delete(a.idemLocks, key)
		}
		a.idemLocksMu.Unlock()
	}
}

// idempotent ejecuta fn una sola vez por Idempotency-Key. Un reintento con el
// mismo cuerpo devuelve la respuesta original (o el trabajo activo en 202 si
// aún no termina); con otro cuerpo responde 409. El lock es por clave, así que
// varios integradores no se bloquean entre sí.
func (a *HTTPAPI) idempotent(w http.ResponseWriter, r *http.Request, fn func(hash string, async bool) (int, any), hash string, async bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		status, payload := fn(hash, async)
		writeJSON(w, status, payload)
		return
	}
	if len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key excede 128 caracteres", nil)
		return
	}

	// Trabajo activo (queued/printing/paused) de la misma clave: el reintento
	// ve su estado sin esperar ni crear una segunda factura.
	if job, active, err := a.store.ActiveJobByIdemKey(r.Context(), key); err != nil {
		a.log.Error("buscar trabajo activo de idempotencia", "error", err)
	} else if active {
		if job.RequestHash != hash {
			writeError(w, http.StatusConflict, "idempotency_conflict",
				"la clave ya se usó con otro cuerpo", nil)
			return
		}
		writeJSON(w, http.StatusAccepted, jobAcceptedJSON{JobID: job.ID, Status: job.Status, Station: job.Station, Lines: len(job.Lines)})
		return
	}

	unlock := a.lockIdemKey(key)
	defer unlock()

	if storedHash, status, stored, ok, err := a.store.IdempotencyGet(key); err != nil {
		a.log.Error("leer idempotencia", "error", err)
	} else if ok {
		if storedHash != hash {
			writeError(w, http.StatusConflict, "idempotency_conflict",
				"la clave ya se usó con otro cuerpo", nil)
			return
		}
		w.Header().Set("Idempotent-Replay", "true")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(stored)
		return
	}

	status, payload := fn(hash, async)
	encoded, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "no se pudo serializar la respuesta", nil)
		return
	}
	// Resultados finales (2xx y abortos) reemplazan la marca provisional 202.
	// Los pausados conservan la marca: el reintento seguirá viendo el trabajo.
	if status >= 200 && status < 300 || status == http.StatusBadGateway {
		if err := a.store.IdempotencyReplace(key, status, encoded); err != nil {
			a.log.Error("guardar idempotencia", "error", err)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// ---------- validación y errores ----------

// validateStation devuelve (0, "") si la estación es válida; si no, un estado
// HTTP y el mensaje. Si la petición se autenticó con el token de una estación,
// sólo puede emitir en su propio nombre; y una estación con token propio no se
// representa con el token compartido.
func (a *HTTPAPI) validateStation(ctx context.Context, station string) (int, string) {
	station = strings.TrimSpace(station)
	if station == "" {
		return http.StatusBadRequest, "station es obligatorio (ej. \"ECOM-01\")"
	}
	if !config.ValidStationID(station) {
		return http.StatusBadRequest, "station sólo admite letras, números, - _ . : (máx. 64)"
	}
	if bound, ok := stationFromContext(ctx); ok {
		if bound != station {
			return http.StatusForbidden,
				fmt.Sprintf("el token de la estación %q no puede emitir como %q", bound, station)
		}
	} else if _, has := a.cfg.StationTokens[station]; has {
		return http.StatusForbidden,
			fmt.Sprintf("la estación %q exige su token propio (station_tokens)", station)
	}
	if len(a.allowed) > 0 {
		if _, ok := a.allowed[station]; !ok {
			return http.StatusForbidden, fmt.Sprintf("estación %q no autorizada", station)
		}
	}
	return 0, ""
}

func (a *HTTPAPI) timeout(ms int) time.Duration {
	if ms <= 0 {
		return a.cfg.DefaultTimeout
	}
	d := time.Duration(ms) * time.Millisecond
	if a.cfg.MaxTimeout > 0 && d > a.cfg.MaxTimeout {
		d = a.cfg.MaxTimeout
	}
	return d
}

// spoolerStatus ya no existe como función: la clasificación vive en
// spoolerError y cada llamador arma su envoltorio con detalles propios.

func statusCode(status int) string {
	switch status {
	case http.StatusForbidden:
		return "station_not_allowed"
	case http.StatusBadRequest:
		return "invalid_request"
	default:
		return "error"
	}
}

// errorDetails viaja dentro del error para que el integrador sepa qué pasó.
type errorDetails struct {
	LineIndex     *int   `json:"line_index,omitempty"`
	JobID         string `json:"job_id,omitempty"`
	DocumentOpen  bool   `json:"document_open"`
	DocumentOwner string `json:"document_owner,omitempty"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	*errorDetails
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func errorEnvelopeOf(code, msg string, details *errorDetails) errorEnvelope {
	if details == nil {
		details = &errorDetails{}
	}
	return errorEnvelope{Error: errorBody{Code: code, Message: msg, errorDetails: details}}
}

func writeError(w http.ResponseWriter, status int, code, msg string, details *errorDetails) {
	writeJSON(w, status, errorEnvelopeOf(code, msg, details))
}

// ---------- middleware ----------

// stationTokenKey identifica la estación a la que está atado un token propio.
type stationTokenKey struct{}

// stationFromContext devuelve la estación vinculada al token de la petición.
// No hay valor cuando se autenticó con el token compartido.
func stationFromContext(ctx context.Context) (string, bool) {
	st, ok := ctx.Value(stationTokenKey{}).(string)
	return st, ok && st != ""
}

func (a *HTTPAPI) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		station, ok := a.authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="spooler-fiscal"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "token inválido o ausente", nil)
			return
		}
		if station != "" {
			r = r.WithContext(context.WithValue(r.Context(), stationTokenKey{}, station))
		}
		next(w, r)
	}
}

// authenticate devuelve la estación vinculada al token ("" para el token
// compartido) y si el token es válido. Todos los tokens se comparan en tiempo
// constante; no se corta el bucle en la primera coincidencia.
func (a *HTTPAPI) authenticate(r *http.Request) (string, bool) {
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(got) > 7 && strings.EqualFold(got[:7], "bearer ") {
		got = strings.TrimSpace(got[7:])
	}
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("X-Spooler-Token"))
	}
	if got == "" {
		return "", false
	}
	if want := a.cfg.Token; want != "" &&
		subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
		return "", true
	}
	want := []byte(got)
	bound, found := "", false
	for station, token := range a.cfg.StationTokens {
		if token == "" {
			continue
		}
		if subtle.ConstantTimeCompare(want, []byte(token)) == 1 {
			bound, found = station, true
		}
	}
	return bound, found
}

func (a *HTTPAPI) decode(w http.ResponseWriter, r *http.Request, dst any) ([]byte, bool) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type debe ser application/json", nil)
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.cfg.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
			fmt.Sprintf("cuerpo mayor de %d bytes", a.cfg.MaxBodyBytes), nil)
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "cuerpo JSON vacío", nil)
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "JSON inválido: "+err.Error(), nil)
		return nil, false
	}
	return body, true
}

func (a *HTTPAPI) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				a.log.Error("pánico en la API REST", "ruta", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal_error", "error interno", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *HTTPAPI) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.log.Info("api rest", "metodo", r.Method, "ruta", r.URL.Path,
			"estado", rec.status, "ms", time.Since(started).Milliseconds(), "remoto", r.RemoteAddr)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// ---------- JSON ----------

type commandRequest struct {
	Station       string `json:"station"`
	Command       string `json:"command,omitempty"`
	CommandBase64 string `json:"command_base64,omitempty"`
	ClientRef     string `json:"client_ref,omitempty"`
	TimeoutMS     int    `json:"timeout_ms,omitempty"`
}

type invoiceRequest struct {
	Station   string   `json:"station"`
	Lines     []string `json:"lines"`
	ClientRef string   `json:"client_ref,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
}

type commandJSON struct {
	Station        string            `json:"station"`
	Session        string            `json:"session"`
	Response       string            `json:"response"`
	ResponseBase64 string            `json:"response_base64"`
	Sequence       int64             `json:"sequence"`
	QueueWaitMS    int64             `json:"queue_wait_ms"`
	LatencyMS      int64             `json:"latency_ms"`
	Status         printerStatusJSON `json:"status"`
	Document       *documentJSON     `json:"document,omitempty"`
}

type lineJSON struct {
	Index       int    `json:"index"`
	Response    string `json:"response"`
	Sequence    int64  `json:"sequence"`
	QueueWaitMS int64  `json:"queue_wait_ms"`
	LatencyMS   int64  `json:"latency_ms"`
}

type invoiceResponse struct {
	Station       string        `json:"station"`
	Session       string        `json:"session"`
	OrderRef      string        `json:"order_ref,omitempty"`
	Completed     bool          `json:"completed"`
	JobID         string        `json:"job_id,omitempty"`
	InvoiceNumber int64         `json:"invoice_number,omitempty"`
	DocumentType  string        `json:"document_type,omitempty"`
	Sequence      int64         `json:"sequence,omitempty"`
	IssuedAt      string        `json:"issued_at,omitempty"`
	Duplicate     bool          `json:"duplicate"`
	Lines         []lineJSON    `json:"lines"`
	Document      *documentJSON `json:"document,omitempty"`
	ElapsedMS     int64         `json:"elapsed_ms"`
}

type documentJSON struct {
	Sequence      int64  `json:"sequence"`
	Type          string `json:"type"`
	Number        int64  `json:"number"`
	StationID     string `json:"station"`
	SessionID     string `json:"session"`
	IssuedAt      string `json:"issued_at"`
	ResponseText  string `json:"response_text"`
	Duplicate     bool   `json:"duplicate"`
	ClientRef     string `json:"client_ref,omitempty"`
	RequestSHA256 string `json:"request_sha256"`
	ResponseSHA   string `json:"response_sha256"`
}

func documentJSONOf(d *DocumentRecord) *documentJSON {
	if d == nil {
		return nil
	}
	return &documentJSON{
		Sequence: d.Sequence, Type: d.Type, Number: d.Number,
		StationID: d.StationID, SessionID: d.SessionID,
		IssuedAt:     d.IssuedAt.UTC().Format(time.RFC3339),
		ResponseText: d.ResponseText, Duplicate: d.Duplicate, ClientRef: d.ClientRef,
		RequestSHA256: d.RequestSHA256, ResponseSHA: d.ResponseSHA256,
	}
}

type printerStatusJSON struct {
	Online           bool   `json:"online"`
	StatusByte       uint32 `json:"status_byte"`
	ErrorByte        uint32 `json:"error_byte"`
	StatusText       string `json:"status_text"`
	ErrorText        string `json:"error_text"`
	PaperOut         bool   `json:"paper_out"`
	FiscalMemoryFull bool   `json:"fiscal_memory_full"`
	MemoryAlmostFull bool   `json:"fiscal_memory_almost_full"`
	LastError        string `json:"last_error,omitempty"`
	Raw              string `json:"raw,omitempty"`
	CheckedAt        int64  `json:"checked_at_unix"`
}

func printerStatusJSONOf(st fiscal.Status) printerStatusJSON {
	return printerStatusJSON{
		Online: st.Online, StatusByte: uint32(st.StatusByte), ErrorByte: uint32(st.ErrorByte),
		StatusText: st.StatusText, ErrorText: st.ErrorText, PaperOut: st.PaperOut,
		FiscalMemoryFull: st.MemoryFull, MemoryAlmostFull: st.MemoryAlmostFull,
		LastError: st.LastError, Raw: st.Raw, CheckedAt: st.Checked.Unix(),
	}
}

type statusJSON struct {
	Printer            printerStatusJSON `json:"printer"`
	Online             bool              `json:"online"`
	Blocked            bool              `json:"blocked"`
	BlockedReason      string            `json:"blocked_reason,omitempty"`
	QueueDepth         int               `json:"queue_depth"`
	DocumentOpen       bool              `json:"document_open"`
	DocumentOwner      string            `json:"document_owner,omitempty"`
	DocumentsIssued    uint64            `json:"documents_issued"`
	CommandsTotal      uint64            `json:"commands_total"`
	CommandsFailed     uint64            `json:"commands_failed"`
	DuplicatesDetected uint64            `json:"duplicates_detected"`
	AvgLatencyMS       float64           `json:"avg_latency_ms"`
	MaxLatencyMS       float64           `json:"max_latency_ms"`
	UptimeSeconds      int64             `json:"uptime_seconds"`
	LastSequence       int64             `json:"last_sequence"`
	HistoryCount       int64             `json:"history_count"`
	LastInvoiceNumber  int64             `json:"last_invoice_number"`
	LastDocument       *documentJSON     `json:"last_document,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "si", "sí":
		return true
	default:
		return false
	}
}
