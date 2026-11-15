package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"spoolerfiscal/internal/fiscal"
)

// Errores de la cola.
var (
	ErrQueueFull          = errors.New("spooler: cola de impresión llena, reintente")
	ErrClosed             = errors.New("spooler: servidor detenido")
	ErrPrinterUnavailable = errors.New("spooler: impresora no disponible")
	ErrPrinterTimeout     = errors.New("spooler: la impresora no respondió")
)

// BlockedError indica que el guardián impide emitir en el estado actual.
type BlockedError struct {
	Reason string
}

func (e *BlockedError) Error() string {
	return "spooler: emisión bloqueada: " + e.Reason
}

// GuardConfig define cuándo se bloquean las emisiones.
type GuardConfig struct {
	BlockOnPaperOut   bool
	BlockOnMemoryFull bool
	BlockOnError      bool
	BlockWhenOffline  bool
}

// SpoolerConfig configura la cola y el guardián.
type SpoolerConfig struct {
	MaxPending     int
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	ProbeTimeout   time.Duration
	OpenRetry      time.Duration
	// DocumentLockTimeout libera el turno de una factura abandonada (Profit
	// Plus cerrado a mitad de emisión). 0 = nunca liberar.
	DocumentLockTimeout time.Duration
	// CorrelativeMaxAge define qué tan vieja puede estar la última consulta S1
	// antes de que el planificador inserte una conciliación previa a abrir una
	// factura. 0 = 10 segundos.
	CorrelativeMaxAge time.Duration
	// AutoCloseOpenDocument permite al spooler enviar 101 de recuperación cuando
	// la impresora reporta un documento abierto que ninguna sesión reclama.
	AutoCloseOpenDocument bool
	Guard                 GuardConfig
	LogCommands           bool
	// CommandTerminator se añade a los comandos que no lo traen (API REST,
	// integraciones propias). Profit Plus ya envía el suyo y se normaliza
	// para no duplicarlo. Vacío = no tocar el comando.
	CommandTerminator string
}

// SubmitRequest es una petición de ejecución en la cola.
type SubmitRequest struct {
	Command   []byte
	Station   string
	Session   string
	ClientRef string
	Timeout   time.Duration
	// BypassGuard omite el guardián de estado. Sólo para comandos internos de
	// recuperación (el 101 que cierra un documento abandonado): sin él, el
	// guardián bloquearía la cura del propio impedimento.
	BypassGuard bool
}

// Result es el resultado de ejecutar un comando.
type Result struct {
	Response  []byte
	Status    fiscal.Status
	Document  *DocumentRecord
	Sequence  int64
	QueueWait time.Duration
	Latency   time.Duration
	Info      fiscal.Info
	Err       error
}

type jobKind int

const (
	kindCommand jobKind = iota
	kindStatus
	kindInfo
	kindSync
)

type job struct {
	kind      jobKind
	cmd       []byte
	station   string
	session   string
	clientRef string
	received  time.Time
	deadline  time.Time
	res       chan Result
}

// Counters agrupa las métricas básicas del spooler.
type Counters struct {
	Documents    uint64
	Commands     uint64
	Failures     uint64
	Duplicates   uint64
	StoreFailure uint64
	LatencySumMS uint64
	LatencyMaxMS uint64
}

// Snapshot es la foto consistente del estado del spooler.
type Snapshot struct {
	Status        fiscal.Status
	Info          fiscal.Info
	QueueDepth    int
	Counters      Counters
	AvgLatency    float64
	Uptime        time.Duration
	LastSeq       int64
	History       History
	Blocked       string
	DocumentOpen  bool
	DocumentOwner string
}

// Spooler es el único escritor de la impresora fiscal.
//
// La cola no es un simple FIFO de comandos: cada estación tiene su propia cola
// y el planificador le da el turno a una sola estación por vez. Cuando una
// estación abre una factura (primera línea de ítem), conserva el turno hasta
// cerrarla, de modo que dos facturas de estaciones distintas nunca se
// entrelazan en el papel.
type Spooler struct {
	printer fiscal.Printer
	store   *Store
	cfg     SpoolerConfig
	log     *slog.Logger
	now     func() time.Time

	pending atomic.Int64
	execSeq atomic.Int64

	schedMu     sync.Mutex
	cond        *sync.Cond
	queues      map[string][]*job
	order       []string
	inOrder     map[string]bool
	probes      []*job
	docOpen     bool
	docOwner    string
	docSince    time.Time
	schedClosed bool
	// correlativeAt es la hora de la última conciliación S1 con la impresora.
	// Se maneja bajo schedMu porque el planificador decide con ella.
	correlativeAt time.Time

	mu           sync.RWMutex
	status       fiscal.Status
	info         fiscal.Info
	history      History
	correlative  map[string]int64
	syncPending  atomic.Bool

	cDocuments    atomic.Uint64
	cCommands     atomic.Uint64
	cFailures     atomic.Uint64
	cDuplicates   atomic.Uint64
	cStoreFailure atomic.Uint64
	cLatencySum   atomic.Uint64
	cLatencyMax   atomic.Uint64

	baseCtx   context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startedAt time.Time
	closedFlg atomic.Bool
}

// NewSpooler construye el spooler.
func NewSpooler(printer fiscal.Printer, store *Store, cfg SpoolerConfig, log *slog.Logger) *Spooler {
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 64
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 30 * time.Second
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = 2 * time.Minute
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 5 * time.Second
	}
	if cfg.OpenRetry <= 0 {
		cfg.OpenRetry = 300 * time.Millisecond
	}
	if cfg.CorrelativeMaxAge <= 0 {
		cfg.CorrelativeMaxAge = 10 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Spooler{
		printer: printer, store: store, cfg: cfg, log: log, now: time.Now,
		queues:      map[string][]*job{},
		inOrder:     map[string]bool{},
		correlative: map[string]int64{},
		startedAt:   time.Now(),
		info:        fiscal.Info{Brand: printer.Driver()},
	}
	s.cond = sync.NewCond(&s.schedMu)
	s.status.Checked = s.now()
	return s
}

// Start arranca el worker único y carga el historial persistido.
func (s *Spooler) Start(ctx context.Context) error {
	s.baseCtx, s.cancel = context.WithCancel(ctx)
	h, err := s.store.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("cargar historial: %w", err)
	}
	s.mu.Lock()
	s.history = h
	s.mu.Unlock()
	if h.LastSequence > 0 {
		s.execSeq.Store(h.LastSequence)
	}
	if info, ok := s.store.LoadPrinterInfo(ctx); ok {
		s.mu.Lock()
		s.info = info
		s.mu.Unlock()
	}
	// El correlativo arranca de la auditoría propia y de lo que la impresora
	// reportó en conciliaciones anteriores. La primera conciliación S1 en vivo
	// corrige cualquier diferencia: la impresora es la fuente legal.
	if stateCorrelatives, err := s.store.Correlatives(ctx); err == nil {
		s.mu.Lock()
		for t, n := range stateCorrelatives {
			if _, ok := s.correlative[t]; !ok || n > s.correlative[t] {
				s.correlative[t] = n
			}
		}
		s.mu.Unlock()
	} else {
		s.log.Warn("cargar correlativos persistidos", "error", err)
	}
	s.mu.Lock()
	for t, n := range h.LastNumbers {
		if _, ok := s.correlative[t]; !ok || n > s.correlative[t] {
			s.correlative[t] = n
		}
	}
	s.mu.Unlock()
	s.log.Info("historial cargado", "documentos", h.Count, "ultima_secuencia", h.LastSequence,
		"ultima_factura", h.LastNumbers[fiscal.DocInvoice])
	s.mu.RLock()
	s.log.Info("correlativo inicial",
		"ultima_factura", s.correlative[fiscal.DocInvoice],
		"ultima_nc", s.correlative[fiscal.DocCredit],
		"ultima_nd", s.correlative[fiscal.DocDebit])
	s.mu.RUnlock()
	s.wg.Add(1)
	go s.worker()
	s.log.Info("cola de impresión iniciada", "max_pendientes", s.cfg.MaxPending,
		"driver", s.printer.Driver(), "turnos_por_estacion", true)
	// Conciliación inicial en segundo plano: si la impresora tiene documentos
	// emitidos fuera del spooler (o un corte a mitad), el número real se carga
	// antes de la primera factura nueva.
	s.TriggerCorrelativeSync()
	return nil
}

// Stop detiene el worker y cierra la impresora.
func (s *Spooler) Stop() {
	s.closedFlg.Store(true)
	s.schedMu.Lock()
	s.schedClosed = true
	s.cond.Broadcast()
	s.schedMu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if err := s.printer.Close(); err != nil {
		s.log.Warn("cerrar impresora", "error", err)
	}
}

func (s *Spooler) worker() {
	defer s.wg.Done()
	for {
		j := s.nextJob()
		if j == nil {
			return
		}
		s.execute(j)
	}
}

// nextJob entrega el próximo trabajo ejecutable. Bloquea hasta que haya uno.
// Si hay una factura abierta, sólo se atienden los comandos de su estación
// (y las sondas de estado, que son de sólo lectura).
func (s *Spooler) nextJob() *job {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()

	for {
		if s.schedClosed {
			return nil
		}
		// Un ítem de factura vencido no debe imprimirse tarde ni fuera de su
		// documento: se descarta antes de elegir el próximo trabajo.
		s.sweepExpiredLocked()

		if len(s.probes) > 0 {
			j := s.probes[0]
			s.probes = s.probes[1:]
			return j
		}

		if s.docOpen {
			if q := s.queues[s.docOwner]; len(q) > 0 {
				if s.shouldSyncBeforeLocked(q[0]) {
					return s.newSyncJobLocked()
				}
				return s.popLocked(s.docOwner)
			}
			if s.cfg.DocumentLockTimeout > 0 && s.now().Sub(s.docSince) > s.cfg.DocumentLockTimeout {
				s.log.Error("factura abandonada: se libera el turno de la estación",
					"estacion", s.docOwner, "desde", s.docSince.Format(time.RFC3339),
					"timeout", s.cfg.DocumentLockTimeout)
				s.docOpen, s.docOwner = false, ""
				s.cond.Broadcast()
				continue
			}
			s.waitLocked()
			continue
		}

		if len(s.order) > 0 {
			id := s.order[0]
			if len(s.queues[id]) == 0 {
				s.order = s.order[1:]
				s.inOrder[id] = false
				continue
			}
			if s.shouldSyncBeforeLocked(s.queues[id][0]) {
				return s.newSyncJobLocked()
			}
			return s.popLocked(id)
		}
		s.waitLocked()
	}
}

// waitLocked duerme hasta que llegue un trabajo o venza el más próximo de los
// retenidos, para poder descartarlo a tiempo.
func (s *Spooler) waitLocked() {
	broadcast := func() {
		s.schedMu.Lock()
		s.cond.Broadcast()
		s.schedMu.Unlock()
	}
	if next := s.nextDeadlineLocked(); !next.IsZero() {
		if d := time.Until(next); d > 0 {
			timer := time.AfterFunc(d+2*time.Millisecond, broadcast)
			defer timer.Stop()
		}
	}
	// Latido: un Broadcast puede caer sin oyentes si el timer dispara antes de
	// que el worker llegue al Wait. Con el latido el worker despierta al menos
	// una vez por intervalo y el barrido de vencidos nunca queda retenido.
	beat := time.AfterFunc(500*time.Millisecond, broadcast)
	defer beat.Stop()
	s.cond.Wait()
}

func (s *Spooler) nextDeadlineLocked() time.Time {
	var next time.Time
	consider := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, q := range s.queues {
		if len(q) > 0 {
			consider(q[0].deadline)
		}
	}
	for _, j := range s.probes {
		consider(j.deadline)
	}
	if s.docOpen && s.cfg.DocumentLockTimeout > 0 {
		consider(s.docSince.Add(s.cfg.DocumentLockTimeout))
	}
	return next
}

// sweepExpiredLocked responde DeadlineExceeded a los trabajos vencidos y los
// quita de la cola, incluso si están retenidos por una factura abierta.
func (s *Spooler) sweepExpiredLocked() {
	now := s.now()
	expired := func(j *job) bool {
		if !now.After(j.deadline) {
			return false
		}
		s.pending.Add(-1)
		j.res <- Result{QueueWait: now.Sub(j.received), Err: context.DeadlineExceeded}
		return true
	}

	for id, q := range s.queues {
		kept := q[:0]
		for _, j := range q {
			if !expired(j) {
				kept = append(kept, j)
			}
		}
		if len(kept) == 0 {
			delete(s.queues, id)
		} else {
			s.queues[id] = kept
		}
	}

	keptOrder := s.order[:0]
	for _, id := range s.order {
		if len(s.queues[id]) > 0 {
			keptOrder = append(keptOrder, id)
			s.inOrder[id] = true
		} else {
			s.inOrder[id] = false
		}
	}
	s.order = keptOrder

	if len(s.probes) > 0 {
		kept := s.probes[:0]
		for _, j := range s.probes {
			if !expired(j) {
				kept = append(kept, j)
			}
		}
		s.probes = kept
	}
}

func (s *Spooler) popLocked(session string) *job {
	q := s.queues[session]
	j := q[0]
	if len(q) <= 1 {
		delete(s.queues, session)
	} else {
		s.queues[session] = q[1:]
	}
	for i, id := range s.order {
		if id == session {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.inOrder[session] = false
	if len(s.queues[session]) > 0 {
		s.order = append(s.order, session)
		s.inOrder[session] = true
	}
	return j
}

// Submit encola un comando y espera su ejecución.
func (s *Spooler) Submit(ctx context.Context, req SubmitRequest) (Result, error) {
	if s.closedFlg.Load() {
		return Result{}, ErrClosed
	}
	if len(req.Command) == 0 {
		return Result{}, errors.New("spooler: comando vacío")
	}
	cmd := normalizeCommand(req.Command, s.cfg.CommandTerminator)
	if !req.BypassGuard {
		if reason := s.blockedReason(); reason != "" {
			return Result{}, &BlockedError{Reason: reason}
		}
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = s.cfg.DefaultTimeout
	}
	if timeout > s.cfg.MaxTimeout {
		timeout = s.cfg.MaxTimeout
	}
	now := s.now()
	j := &job{
		kind: kindCommand, cmd: cmd, station: req.Station, session: req.Session,
		clientRef: req.ClientRef, received: now, deadline: now.Add(timeout),
		res: make(chan Result, 1),
	}
	if s.pending.Load() >= int64(s.cfg.MaxPending) {
		return Result{}, ErrQueueFull
	}
	s.pending.Add(1)
	s.schedMu.Lock()
	if s.schedClosed {
		s.schedMu.Unlock()
		s.pending.Add(-1)
		return Result{}, ErrClosed
	}
	s.queues[req.Session] = append(s.queues[req.Session], j)
	if !s.inOrder[req.Session] {
		s.order = append(s.order, req.Session)
		s.inOrder[req.Session] = true
	}
	s.cond.Signal()
	s.schedMu.Unlock()

	select {
	case r := <-j.res:
		return r, r.Err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Step clasifica un comando con el driver activo: apertura o cierre de
// documento. Lo usa la API REST para validar una factura antes de enviarla.
func (s *Spooler) Step(cmd []byte) fiscal.Step { return s.printer.Step(cmd) }

// normalizeCommand recorta los terminadores que ya traiga el comando y añade
// el configurado. Así Profit Plus (`\r`) y la API REST (sin terminador) usan
// exactamente el mismo protocolo sobre el cable.
func normalizeCommand(cmd []byte, ending string) []byte {
	if ending == "" {
		return cmd
	}
	trimmed := bytes.TrimRight(cmd, "\r\n")
	if len(trimmed) == 0 {
		return cmd
	}
	out := make([]byte, len(trimmed)+len(ending))
	copy(out, trimmed)
	copy(out[len(trimmed):], ending)
	return out
}

// enqueueProbe encola una operación de sólo lectura (sonda S2, S1/info o
// conciliación de correlativo) que no altera el documento en curso.
func (s *Spooler) enqueueProbe(j *job) (Result, error) {
	if s.pending.Load() >= int64(s.cfg.MaxPending) {
		return Result{}, ErrQueueFull
	}
	s.pending.Add(1)
	s.schedMu.Lock()
	if s.schedClosed {
		s.schedMu.Unlock()
		s.pending.Add(-1)
		return Result{}, ErrClosed
	}
	s.probes = append(s.probes, j)
	s.cond.Signal()
	s.schedMu.Unlock()

	select {
	case r := <-j.res:
		return r, r.Err
	case <-s.baseCtx.Done():
		return Result{}, ErrClosed
	}
}

// ProbeStatus consulta la impresora fuera del flujo de emisión.
func (s *Spooler) ProbeStatus(ctx context.Context) (fiscal.Status, error) {
	if s.closedFlg.Load() {
		return fiscal.Status{}, ErrClosed
	}
	now := s.now()
	j := &job{kind: kindStatus, received: now, deadline: now.Add(s.cfg.ProbeTimeout), res: make(chan Result, 1)}
	r, err := s.enqueueProbe(j)
	return r.Status, err
}

// ProbeInfo consulta la identificación de la impresora.
func (s *Spooler) ProbeInfo(ctx context.Context) (fiscal.Info, error) {
	if s.closedFlg.Load() {
		return fiscal.Info{}, ErrClosed
	}
	now := s.now()
	j := &job{kind: kindInfo, received: now, deadline: now.Add(s.cfg.ProbeTimeout), res: make(chan Result, 1)}
	r, err := s.enqueueProbe(j)
	return r.Info, err
}

func (s *Spooler) execute(j *job) {
	defer s.pending.Add(-1)
	queueWait := s.now().Sub(j.received)
	ctx, cancel := context.WithDeadline(s.baseCtx, j.deadline)
	defer cancel()

	if err := s.ensureOpen(ctx); err != nil {
		s.cFailures.Add(1)
		j.res <- Result{QueueWait: queueWait, Err: err}
		return
	}

	switch j.kind {
	case kindStatus:
		st, err := s.printer.GetStatus(ctx)
		s.updateStatus(st)
		if err != nil {
			s.cFailures.Add(1)
		}
		j.res <- Result{Status: st, QueueWait: queueWait, Err: err}
		return
	case kindInfo:
		info, err := s.printer.GetInfo(ctx)
		if err == nil {
			s.mu.Lock()
			s.info = info
			s.mu.Unlock()
			if serr := s.store.SavePrinterInfo(ctx, info); serr != nil {
				s.log.Warn("persistir datos de impresora", "error", serr)
			}
		} else {
			s.cFailures.Add(1)
		}
		j.res <- Result{Info: info, QueueWait: queueWait, Err: err}
		return
	case kindSync:
		info, err := s.syncCorrelative(ctx)
		if err != nil {
			s.cFailures.Add(1)
			s.log.Warn("conciliación de correlativo falló", "error", err)
		}
		s.schedMu.Lock()
		s.correlativeAt = s.now()
		s.schedMu.Unlock()
		j.res <- Result{Info: info, QueueWait: queueWait, Err: err}
		return
	}

	start := s.now()
	if err := s.printer.SendCommand(ctx, j.cmd); err != nil {
		s.cFailures.Add(1)
		s.log.Error("error escribiendo en la impresora", "estacion", j.station, "error", err)
		j.res <- Result{QueueWait: queueWait, Err: fmt.Errorf("%w: %v", ErrPrinterUnavailable, err)}
		return
	}
	resp, err := s.printer.ReadResponse(ctx)
	if err != nil {
		s.cFailures.Add(1)
		s.log.Error("sin respuesta de la impresora", "estacion", j.station, "error", err,
			"nota", "el comando pudo imprimirse; no se reenvía para evitar doble emisión")
		j.res <- Result{QueueWait: queueWait, Err: fmt.Errorf("%w: %v", ErrPrinterTimeout, err)}
		return
	}

	desc := s.printer.Describe(j.cmd, resp)
	s.updateStatus(desc.Status)
	s.advanceDocument(j, desc)
	seq := s.execSeq.Add(1)
	res := Result{
		Response:  resp,
		Status:    desc.Status,
		Sequence:  seq,
		QueueWait: queueWait,
		Latency:   s.now().Sub(start),
	}

	if desc.Document != nil {
		if desc.Document.Number > 0 {
			s.reconcileDocumentCorrelative(ctx, desc.Document.Type, desc.Document.Number)
		}
		rec := DocumentRecord{
			Type: desc.Document.Type, Number: desc.Document.Number,
			StationID: j.station, SessionID: j.session, IssuedAt: s.now(),
			StatusByte: desc.Status.StatusByte, ErrorByte: desc.Status.ErrorByte,
			RequestSHA256: sha256Hex(j.cmd), ResponseSHA256: sha256Hex(resp),
			ResponseText: strings.TrimSpace(string(resp)), ClientRef: j.clientRef,
		}
		stored, dup, rerr := s.store.RecordDocument(ctx, rec)
		if rerr != nil {
			s.cStoreFailure.Add(1)
			s.log.Error("NO se pudo registrar el documento fiscal", "error", rerr,
				"tipo", rec.Type, "numero", rec.Number, "estacion", rec.StationID)
		} else {
			res.Document = &stored
			if dup {
				s.cDuplicates.Add(1)
				s.log.Error("doble emisión detectada: el número ya estaba registrado",
					"tipo", stored.Type, "numero", stored.Number,
					"estacion_original", stored.StationID, "estacion_actual", rec.StationID)
			} else {
				s.cDocuments.Add(1)
				s.log.Info("documento fiscal registrado", "tipo", stored.Type, "numero", stored.Number,
					"secuencia", stored.Sequence, "estacion", stored.StationID, "total", desc.Document.Total)
			}
			s.refreshHistory(ctx)
		}
	}

	requestHash := sha256Hex(j.cmd)
	if s.cfg.LogCommands {
		if lerr := s.store.LogCommand(ctx, CommandLog{
			At: s.now(), StationID: j.station, SessionID: j.session,
			RequestSHA256: requestHash, ResponseSHA256: sha256Hex(resp), OK: !desc.Failed,
			LatencyMS: res.Latency.Milliseconds(),
		}); lerr != nil {
			s.log.Warn("bitácora de comandos", "error", lerr)
		}
	}

	s.cCommands.Add(1)
	s.addLatency(res.Latency)
	if desc.Failed {
		s.cFailures.Add(1)
		s.log.Warn("la impresora reportó un error", "estacion", j.station,
			"codigo", fmt.Sprintf("0x%02X", desc.ErrorCode), "detalle", desc.ErrorText)
	}
	j.res <- res
}

// shouldSyncBeforeLocked decide si el planificador debe conciliar el
// correlativo con la impresora antes de ejecutar el comando que va a abrir un
// documento. Así la impresora — fuente legal — predomina aunque el sistema
// tenga un correlativo rancio por un corte, reinicio o factura colgada.
func (s *Spooler) shouldSyncBeforeLocked(cand *job) bool {
	if cand == nil || cand.kind != kindCommand {
		return false
	}
	if s.docOpen {
		return false
	}
	if s.printer.Step(cand.cmd) != fiscal.StepOpenDocument {
		return false
	}
	if _, ok := s.printer.(fiscal.CorrelativeSource); !ok {
		return false
	}
	if s.correlativeAt.IsZero() {
		return true
	}
	return s.now().Sub(s.correlativeAt) > s.cfg.CorrelativeMaxAge
}

func (s *Spooler) newSyncJobLocked() *job {
	now := s.now()
	return &job{kind: kindSync, received: now, deadline: now.Add(s.cfg.ProbeTimeout), res: make(chan Result, 1)}
}

// correlativeStale informa si la última conciliación está vencida.
func (s *Spooler) correlativeStale() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.correlativeStaleLocked()
}

// correlativeStaleLocked informa si la última conciliación está vencida.
func (s *Spooler) correlativeStaleLocked() bool {
	if _, ok := s.printer.(fiscal.CorrelativeSource); !ok {
		return false
	}
	if s.correlativeAt.IsZero() {
		return true
	}
	return s.now().Sub(s.correlativeAt) > s.cfg.CorrelativeMaxAge
}

// SyncCorrelative concilia el correlativo con la impresora (S1) y devuelve el
// mapa de últimos números emitidos por tipo. Con force=false sólo consulta si
// la última conciliación está vencida. La API REST lo llama antes de la primera
// línea de cada factura; el planificador lo inyecta para las estaciones gRPC.
func (s *Spooler) SyncCorrelative(ctx context.Context, force bool) (map[string]int64, error) {
	if _, ok := s.printer.(fiscal.CorrelativeSource); !ok {
		return s.Correlatives(), nil
	}
	if !force && !s.correlativeStale() {
		return s.Correlatives(), nil
	}
	now := s.now()
	j := &job{kind: kindSync, received: now, deadline: now.Add(s.cfg.ProbeTimeout), res: make(chan Result, 1)}
	if _, err := s.enqueueProbe(j); err != nil {
		return s.Correlatives(), err
	}
	return s.Correlatives(), nil
}

// TriggerCorrelativeSync lanza una conciliación en segundo plano si hay una
// pendiente. Lo usan el arranque del spooler y la apertura de sesión de una
// estación: para cuando llegue la primera factura el correlativo ya está vivo.
func (s *Spooler) TriggerCorrelativeSync() {
	if _, ok := s.printer.(fiscal.CorrelativeSource); !ok {
		return
	}
	if s.baseCtx == nil || s.syncPending.Swap(true) {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.syncPending.Store(false)
		if s.closedFlg.Load() {
			return
		}
		if _, err := s.SyncCorrelative(s.baseCtx, false); err != nil {
			s.log.Warn("conciliación de correlativo en segundo plano", "error", err)
		}
	}()
}

// Correlatives devuelve una copia del último número conocido por tipo.
func (s *Spooler) Correlatives() map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]int64, len(s.correlative))
	for t, n := range s.correlative {
		out[t] = n
	}
	return out
}

// ExpectedCorrelative es el siguiente número que debería emitir la impresora
// para ese tipo de documento, según la última conciliación.
func (s *Spooler) ExpectedCorrelative(docType string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.correlative[docType] + 1
}

// syncCorrelative consulta S1 al driver y ajusta el estado interno y la BD.
// La impresora siempre gana, incluso con un número menor (ej. tras un Z).
func (s *Spooler) syncCorrelative(ctx context.Context) (fiscal.Info, error) {
	src, ok := s.printer.(fiscal.CorrelativeSource)
	if !ok {
		s.mu.RLock()
		info := s.info
		s.mu.RUnlock()
		return info, nil
	}
	info, err := src.GetCorrelative(ctx)
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrPrinterUnavailable, err)
	}
	s.mu.Lock()
	merged := s.info
	if merged.Brand == "" {
		merged.Brand = info.Brand
	}
	merged.Serial = info.Serial
	merged.RIF = info.RIF
	if info.Port != "" {
		merged.Port = info.Port
	}
	merged.LastInvoice = info.LastInvoice
	merged.LastCredit = info.LastCredit
	merged.LastDebit = info.LastDebit
	s.info = merged
	updates := map[string]int64{
		fiscal.DocInvoice: info.LastInvoice,
		fiscal.DocCredit:  info.LastCredit,
		fiscal.DocDebit:   info.LastDebit,
	}
	for t, n := range updates {
		if n <= 0 {
			continue
		}
		if prev := s.correlative[t]; prev > 0 && prev != n {
			s.log.Warn("correlativo conciliado con la impresora: predomina la impresora",
				"tipo", t, "sistema", prev, "impresora", n)
		}
		s.correlative[t] = n
	}
	s.mu.Unlock()
	for t, n := range updates {
		if n > 0 {
			if err := s.store.SetCorrelative(ctx, t, n, "printer"); err != nil {
				s.log.Warn("persistir correlativo conciliado", "tipo", t, "error", err)
			}
		}
	}
	return merged, nil
}

// reconcileDocumentCorrelative ajusta el correlativo con el número real que
// acaba de imprimir la impresora y registra huecos o repeticiones.
func (s *Spooler) reconcileDocumentCorrelative(ctx context.Context, docType string, number int64) {
	s.mu.Lock()
	prev := s.correlative[docType]
	s.correlative[docType] = number
	s.mu.Unlock()
	if prev > 0 {
		switch {
		case number <= prev:
			s.log.Error("correlativo: la impresora emitió un número no consecutivo (posible doble emisión o reinicio)",
				"tipo", docType, "esperado_mayor_que", prev, "impresora", number)
		case number > prev+1:
			s.log.Warn("correlativo: hueco detectado entre el sistema y la impresora",
				"tipo", docType, "esperado", prev+1, "impresora", number)
		}
	}
	if err := s.store.SetCorrelative(ctx, docType, number, "printer"); err != nil {
		s.log.Warn("persistir correlativo", "tipo", docType, "error", err)
	}
}

// RecoverOpenDocument consulta el estado físico; si hay un documento fiscal
// abierto que ninguna sesión viva del spooler reclama (factura colgada),
// envía el cierre 101 de recuperación. owner es la sesión que reclama el
// turno como legítimo dueño (vacío = nadie); si el dueño vive, no interviene.
func (s *Spooler) RecoverOpenDocument(ctx context.Context, owner string) (bool, error) {
	if !s.cfg.AutoCloseOpenDocument {
		return false, nil
	}
	if s.closedFlg.Load() {
		return false, ErrClosed
	}
	st, err := s.ProbeStatus(ctx)
	if err != nil {
		return false, err
	}
	if !st.DocumentOpen {
		return false, nil
	}
	s.schedMu.Lock()
	docOpen, docOwner := s.docOpen, s.docOwner
	s.schedMu.Unlock()
	if docOpen && docOwner != owner {
		return false, nil
	}
	s.log.Warn("documento fiscal abierto sin dueño: se envía cierre de recuperación (101)",
		"status", st.StatusText, "raw", st.Raw)
	// El 101 pertenece al documento de la sesión colgada: se envía con su
	// identidad para que el planificador lo atienda dentro de su propio turno
	// (y para que la auditoría registre quién cerró la factura).
	session, station := "recuperacion", "recuperacion"
	if owner != "" {
		session, station = owner, owner
	}
	if _, err := s.Submit(ctx, SubmitRequest{
		Command:     []byte("101"),
		Station:     station,
		Session:     session,
		Timeout:     s.cfg.DefaultTimeout,
		BypassGuard: true,
	}); err != nil {
		return false, fmt.Errorf("cierre de recuperación: %w", err)
	}
	return true, nil
}

// advanceDocument mantiene el turno de la estación que tiene una factura
// abierta. Sólo cambia de estado cuando el comando llegó a ejecutarse.
func (s *Spooler) advanceDocument(j *job, desc fiscal.Description) {
	step := s.printer.Step(j.cmd)
	if step == fiscal.StepNeutral {
		return
	}
	s.schedMu.Lock()
	switch step {
	case fiscal.StepOpenDocument:
		if !s.docOpen {
			s.docOpen, s.docOwner, s.docSince = true, j.session, s.now()
			s.log.Debug("factura abierta: turno exclusivo", "estacion", j.station)
		}
	case fiscal.StepCloseDocument:
		if s.docOpen {
			s.log.Debug("factura cerrada: se libera el turno", "estacion", j.station)
		}
		s.docOpen, s.docOwner = false, ""
	}
	s.cond.Broadcast()
	s.schedMu.Unlock()
}

func (s *Spooler) ensureOpen(ctx context.Context) error {
	var lastErr error
	for {
		if err := s.printer.Open(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		s.updateStatus(fiscal.Status{Online: false, LastError: lastErr.Error(), Checked: s.now()})
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrPrinterUnavailable, lastErr)
		case <-time.After(s.cfg.OpenRetry):
			s.log.Warn("reintentando abrir la impresora", "error", lastErr)
		}
	}
}

func (s *Spooler) updateStatus(st fiscal.Status) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

func (s *Spooler) refreshHistory(ctx context.Context) {
	h, err := s.store.Snapshot(ctx)
	if err != nil {
		s.log.Warn("actualizar historial en memoria", "error", err)
		return
	}
	s.mu.Lock()
	s.history = h
	s.mu.Unlock()
}

func (s *Spooler) addLatency(d time.Duration) {
	ms := uint64(d.Milliseconds())
	s.cLatencySum.Add(ms)
	for {
		cur := s.cLatencyMax.Load()
		if ms <= cur || s.cLatencyMax.CompareAndSwap(cur, ms) {
			return
		}
	}
}

// blockedReason aplica el guardián de estado antes de emitir.
func (s *Spooler) blockedReason() string {
	g := s.cfg.Guard
	s.mu.RLock()
	st := s.status
	s.mu.RUnlock()
	if g.BlockWhenOffline && !st.Online {
		return "PRINTER_OFFLINE"
	}
	if g.BlockOnPaperOut && st.PaperOut {
		return "PAPER_OUT"
	}
	if g.BlockOnMemoryFull && st.MemoryFull {
		return "FISCAL_MEMORY_FULL"
	}
	if g.BlockOnError && hardwareError(st.ErrorByte) {
		return fmt.Sprintf("PRINTER_ERROR_0x%02X", st.ErrorByte)
	}
	// Documento fiscal abierto en la impresora que ninguna sesión del spooler
	// reclama: una factura colgada. Emitir encima entremezclaría ítems.
	// Se cura con RecoverOpenDocument (101) y el estado se refresca con la
	// respuesta del cierre.
	s.schedMu.Lock()
	docOpen := s.docOpen
	s.schedMu.Unlock()
	if st.DocumentOpen && !docOpen {
		return "DOCUMENT_OPEN_RECOVERY"
	}
	return ""
}

// hardwareError distingue fallas físicas/fiscales de errores de comando, que no
// deben bloquear la operación.
func hardwareError(b byte) bool {
	switch b {
	case 0x01, 0x02, 0x03, 0x60, 0x64, 0x6C, 0x80, 0x89, 0x90:
		return true
	}
	return false
}

// Snapshot devuelve el estado consolidado del spooler.
func (s *Spooler) Snapshot() Snapshot {
	s.mu.RLock()
	st := s.status
	info := s.info
	h := s.history
	s.mu.RUnlock()

	s.schedMu.Lock()
	docOpen, docOwner := s.docOpen, s.docOwner
	s.schedMu.Unlock()

	cmds := s.cCommands.Load()
	var avg float64
	if cmds > 0 {
		avg = float64(s.cLatencySum.Load()) / float64(cmds)
	}
	return Snapshot{
		Status: st, Info: info, QueueDepth: int(s.pending.Load()),
		Counters: Counters{
			Documents: s.cDocuments.Load(), Commands: cmds, Failures: s.cFailures.Load(),
			Duplicates: s.cDuplicates.Load(), StoreFailure: s.cStoreFailure.Load(),
			LatencySumMS: s.cLatencySum.Load(), LatencyMaxMS: s.cLatencyMax.Load(),
		},
		AvgLatency: avg, Uptime: s.now().Sub(s.startedAt), LastSeq: s.execSeq.Load(),
		History: h, Blocked: s.blockedReason(),
		DocumentOpen: docOpen, DocumentOwner: docOwner,
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
