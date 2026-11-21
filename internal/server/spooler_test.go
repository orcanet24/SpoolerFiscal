package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"spoolerfiscal/internal/fiscal"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakePrinter es una impresora fiscal de prueba a nivel de protocolo.
type fakePrinter struct {
	interpreter fiscal.Printer

	mu       sync.Mutex
	status   fiscal.Status
	commands [][]byte
	busy     bool
	overlap  bool
	openErr  error
	delay    time.Duration
	docSeq   int64
	openDoc  bool // simulación del estado físico: documento abierto en la impresora
	failSend error
	failRead error
	// failAfterN hace que ReadResponse falle UNA VEZ a partir del comando N+1:
	// simula un corte a mitad de factura; la recuperación posterior funciona.
	// 0 = nunca falla por conteo.
	failAfterN int
	failedOnce bool
}

func newFakePrinter(t *testing.T) *fakePrinter {
	t.Helper()
	ip, err := fiscal.New("tfhka", fiscal.Options{Logger: testLogger()})
	if err != nil {
		t.Fatalf("fiscal.New: %v", err)
	}
	return &fakePrinter{interpreter: ip}
}

func (f *fakePrinter) Driver() string { return "tfhka" }
func (f *fakePrinter) Open(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openErr
}
func (f *fakePrinter) Close() error { return nil }

func (f *fakePrinter) SendCommand(ctx context.Context, cmd []byte) error {
	f.mu.Lock()
	if f.busy {
		f.overlap = true
	}
	f.busy = true
	f.commands = append(f.commands, append([]byte(nil), cmd...))
	delay, sendErr := f.delay, f.failSend
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			f.release()
			return ctx.Err()
		}
	}
	if sendErr != nil {
		f.release()
		return sendErr
	}
	return nil
}

func (f *fakePrinter) release() {
	f.mu.Lock()
	f.busy = false
	f.mu.Unlock()
}

func (f *fakePrinter) ReadResponse(ctx context.Context) ([]byte, error) {
	defer f.release()
	f.mu.Lock()
	cmd := ""
	if len(f.commands) > 0 {
		cmd = strings.TrimSpace(string(f.commands[len(f.commands)-1]))
	}
	failRead := f.failRead
	shouldFail := f.failAfterN > 0 && len(f.commands) > f.failAfterN && !f.failedOnce
	if shouldFail {
		f.failedOnce = true
	}
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if failRead != nil {
		return nil, failRead
	}
	if shouldFail {
		return nil, fmt.Errorf("corte simulado después del comando %d", f.failAfterN)
	}
	// Simulación del estado físico: el ítem abre el documento y 101/81 lo
	// cierran, igual que la impresora real reporta por S2.
	switch f.interpreter.Step([]byte(cmd)) {
	case fiscal.StepOpenDocument:
		f.mu.Lock()
		f.openDoc = true
		f.mu.Unlock()
	case fiscal.StepCloseDocument:
		f.mu.Lock()
		f.openDoc = false
		f.mu.Unlock()
	}
	switch cmd {
	case "101":
		f.mu.Lock()
		f.docSeq++
		n := f.docSeq
		f.mu.Unlock()
		return []byte(fmt.Sprintf("0|Factura %d|Total: 116.00|Impreso:%d", n, n)), nil
	case "PAPER":
		return []byte("0x04|0x01|0|OK"), nil
	case "BADCMD":
		return []byte("ERROR|0x50|Comando invalido"), nil
	default:
		return []byte("0|OK"), nil
	}
}

func (f *fakePrinter) GetStatus(context.Context) (fiscal.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status
	if f.openDoc {
		st.DocumentOpen = true
	}
	return st, nil
}

func (f *fakePrinter) GetInfo(context.Context) (fiscal.Info, error) {
	return fiscal.Info{Brand: "tfhka", Model: "TFHKA", Serial: "SIM-01", RIF: "J-1"}, nil
}

func (f *fakePrinter) Describe(cmd, resp []byte) fiscal.Description {
	return f.interpreter.Describe(cmd, resp)
}

func (f *fakePrinter) Step(cmd []byte) fiscal.Step { return f.interpreter.Step(cmd) }

func (f *fakePrinter) commandCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commands)
}

func newTestSpooler(t *testing.T, path string, printer fiscal.Printer, cfg SpoolerConfig) (*Spooler, *Store) {
	t.Helper()
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if cfg.MaxPending == 0 {
		cfg.MaxPending = 16
	}
	if cfg.DefaultTimeout == 0 {
		cfg.DefaultTimeout = 5 * time.Second
	}
	if cfg.OpenRetry == 0 {
		cfg.OpenRetry = 10 * time.Millisecond
	}
	sp := NewSpooler(printer, store, cfg, testLogger())
	if err := sp.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		sp.Stop()
		_ = store.Close()
	})
	return sp, store
}

func TestQueueSerializesConcurrentStations(t *testing.T) {
	fp := newFakePrinter(t)
	fp.delay = 2 * time.Millisecond
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{MaxPending: 64})

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := sp.Submit(context.Background(), SubmitRequest{
				Command: []byte(fmt.Sprintf("CMD-%02d", i)),
				Station: fmt.Sprintf("CAJA-%02d", i%3),
				Session: "s",
				Timeout: 10 * time.Second,
			})
			if err != nil {
				errs <- err
				return
			}
			if !strings.HasPrefix(string(res.Response), "0|OK") {
				errs <- fmt.Errorf("respuesta inesperada: %q", res.Response)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("submit: %v", err)
	}

	fp.mu.Lock()
	overlap := fp.overlap
	fp.mu.Unlock()
	if overlap {
		t.Fatal("dos comandos usaron la impresora al mismo tiempo")
	}
	if got := fp.commandCount(); got != 20 {
		t.Fatalf("comandos impresos = %d, se esperaban 20", got)
	}
	if snap := sp.Snapshot(); snap.Counters.Commands != 20 {
		t.Fatalf("contador de comandos = %d", snap.Counters.Commands)
	}
}

func TestQueueRecordsFiscalDocuments(t *testing.T) {
	fp := newFakePrinter(t)
	sp, store := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{})

	for i := 0; i < 3; i++ {
		res, err := sp.Submit(context.Background(), SubmitRequest{
			Command: []byte("101\r"), Station: "CAJA-01", Session: "s", Timeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if res.Document == nil {
			t.Fatal("no se registró el documento fiscal")
		}
		if res.Document.Duplicate {
			t.Fatal("no debía ser duplicado")
		}
		if res.Document.Number != int64(i+1) {
			t.Fatalf("número = %d, se esperaba %d", res.Document.Number, i+1)
		}
		if res.Document.StationID != "CAJA-01" {
			t.Fatalf("estación = %q", res.Document.StationID)
		}
		if res.Document.ResponseSHA256 == "" || res.Document.RequestSHA256 == "" {
			t.Fatal("faltan hashes de auditoría")
		}
	}

	snap, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Count != 3 || snap.LastNumbers[fiscal.DocInvoice] != 3 {
		t.Fatalf("historial = %+v", snap)
	}
	if got := sp.Snapshot().Counters.Documents; got != 3 {
		t.Fatalf("métrica de documentos = %d", got)
	}
}

func TestGuardBlocksEmissionWhenPaperOut(t *testing.T) {
	fp := newFakePrinter(t)
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{
		Guard: GuardConfig{BlockOnPaperOut: true, BlockOnError: true},
	})

	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("PAPER\r"), Station: "CAJA-01", Session: "s", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("consulta de estado: %v", err)
	}

	_, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("101\r"), Station: "CAJA-01", Session: "s", Timeout: 5 * time.Second,
	})
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("se esperaba BlockedError, se obtuvo %v", err)
	}
	if blocked.Reason != "PAPER_OUT" {
		t.Fatalf("motivo = %q", blocked.Reason)
	}
	if got := fp.commandCount(); got != 1 {
		t.Fatalf("no debía enviarse el cierre de factura; comandos = %d", got)
	}
}

func TestPrinterErrorResponseReachesCaller(t *testing.T) {
	fp := newFakePrinter(t)
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{})

	res, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("BADCMD\r"), Station: "CAJA-01", Session: "s", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("el error de la impresora debe viajar como respuesta, no como fallo de transporte: %v", err)
	}
	if !strings.Contains(string(res.Response), "ERROR|0x50") {
		t.Fatalf("respuesta = %q", res.Response)
	}
	if got := sp.Snapshot().Counters.Failures; got != 1 {
		t.Fatalf("errores = %d", got)
	}
}

func TestQueueRejectsWhenFull(t *testing.T) {
	fp := newFakePrinter(t)
	fp.delay = 500 * time.Millisecond
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{MaxPending: 1})

	first := make(chan error, 1)
	go func() {
		_, err := sp.Submit(context.Background(), SubmitRequest{
			Command: []byte("SLOW\r"), Station: "CAJA-01", Session: "s", Timeout: 5 * time.Second,
		})
		first <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for sp.Snapshot().QueueDepth == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := sp.Submit(ctx, SubmitRequest{
		Command: []byte("101\r"), Station: "CAJA-02", Session: "s", Timeout: 5 * time.Second,
	})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("se esperaba ErrQueueFull, se obtuvo %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("el primer comando debía completarse: %v", err)
	}
}

func TestDuplicateEmissionIsFlaggedAfterPrinterNumberReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")

	fp1 := newFakePrinter(t)
	sp1, _ := newTestSpooler(t, path, fp1, SpoolerConfig{})
	res, err := sp1.Submit(context.Background(), SubmitRequest{
		Command: []byte("101\r"), Station: "CAJA-01", Session: "s", Timeout: 5 * time.Second,
	})
	if err != nil || res.Document == nil {
		t.Fatalf("primer documento: %v / %+v", err, res.Document)
	}
	sp1.Stop()

	// La impresora se reinicia y vuelve a numerar desde 1: el spooler debe
	// detectar la doble emisión en lugar de sobrescribir el registro.
	fp2 := newFakePrinter(t)
	sp2, store2 := newTestSpooler(t, path, fp2, SpoolerConfig{})
	res2, err := sp2.Submit(context.Background(), SubmitRequest{
		Command: []byte("101\r"), Station: "CAJA-02", Session: "s2", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("segundo documento: %v", err)
	}
	if res2.Document == nil || !res2.Document.Duplicate {
		t.Fatalf("debía marcar duplicado: %+v", res2.Document)
	}
	if snap := sp2.Snapshot(); snap.Counters.Duplicates != 1 {
		t.Fatalf("duplicados = %d", snap.Counters.Duplicates)
	}
	hist, _ := store2.Snapshot(context.Background())
	if hist.Count != 1 {
		t.Fatalf("documentos registrados = %d", hist.Count)
	}
}

func TestOfflinePrinterKeepsRequestQueuedUntilTimeout(t *testing.T) {
	fp := newFakePrinter(t)
	fp.openErr = errors.New("puerto COM1 ocupado")
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{})

	start := time.Now()
	_, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("101\r"), Station: "CAJA-01", Session: "s", Timeout: 300 * time.Millisecond,
	})
	if !errors.Is(err, ErrPrinterUnavailable) {
		t.Fatalf("se esperaba ErrPrinterUnavailable, se obtuvo %v", err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("no mantuvo la petición en cola (%v)", elapsed)
	}
	if got := fp.commandCount(); got != 0 {
		t.Fatalf("no debía escribir nada con el puerto caído; comandos = %d", got)
	}
}

// TestDocumentLockPreventsInterleaving verifica que una factura abierta
// conserva el turno: otra estación no puede imprimir ítems dentro de ella.
func TestDocumentLockPreventsInterleaving(t *testing.T) {
	fp := newFakePrinter(t)
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{})

	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("!   1.000     10.00CAFE\r"), Station: "CAJA-01", Session: "s1", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("abrir factura: %v", err)
	}
	if snap := sp.Snapshot(); !snap.DocumentOpen || snap.DocumentOwner != "s1" {
		t.Fatalf("no se marcó la factura abierta: %+v", snap)
	}

	other := make(chan error, 1)
	go func() {
		_, err := sp.Submit(context.Background(), SubmitRequest{
			Command: []byte("!   1.000     20.00TE\r"), Station: "CAJA-02", Session: "s2", Timeout: 5 * time.Second,
		})
		other <- err
	}()

	time.Sleep(150 * time.Millisecond)
	if got := fp.commandCount(); got != 1 {
		t.Fatalf("CAJA-02 imprimió con la factura de CAJA-01 abierta (comandos = %d)", got)
	}

	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("101\r"), Station: "CAJA-01", Session: "s1", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("cerrar factura: %v", err)
	}
	if err := <-other; err != nil {
		t.Fatalf("CAJA-02 tras el cierre: %v", err)
	}

	fp.mu.Lock()
	cmds := make([]string, 0, len(fp.commands))
	for _, c := range fp.commands {
		cmds = append(cmds, strings.TrimSpace(string(c)))
	}
	overlap := fp.overlap
	fp.mu.Unlock()

	if len(cmds) != 3 {
		t.Fatalf("comandos = %v", cmds)
	}
	if !strings.Contains(cmds[0], "CAFE") || cmds[1] != "101" || !strings.Contains(cmds[2], "TE") {
		t.Fatalf("orden incorrecto, se entrelazaron las facturas: %v", cmds)
	}
	if overlap {
		t.Fatal("dos comandos usaron la impresora a la vez")
	}
}

func TestStatusAndInfoProbes(t *testing.T) {
	fp := newFakePrinter(t)
	fp.status = fiscal.Status{Online: true, StatusByte: 0x04, PaperOut: false}
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{})

	st, err := sp.ProbeStatus(context.Background())
	if err != nil {
		t.Fatalf("ProbeStatus: %v", err)
	}
	if !st.Online {
		t.Fatal("estado offline inesperado")
	}
	info, err := sp.ProbeInfo(context.Background())
	if err != nil {
		t.Fatalf("ProbeInfo: %v", err)
	}
	if info.Serial != "SIM-01" {
		t.Fatalf("serial = %q", info.Serial)
	}
}

func TestSpoolerNormalizesCommandTerminator(t *testing.T) {
	fp := newFakePrinter(t)
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{CommandTerminator: "\r\n"})

	for _, cmd := range []string{"!   1.000     10.00CAFE", "S1\r", "S2\r\n"} {
		if _, err := sp.Submit(context.Background(), SubmitRequest{
			Command: []byte(cmd), Station: "CAJA-01", Session: "s", Timeout: 5 * time.Second,
		}); err != nil {
			t.Fatalf("Submit(%q): %v", cmd, err)
		}
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	want := []string{"!   1.000     10.00CAFE\r\n", "S1\r\n", "S2\r\n"}
	for i, w := range want {
		if got := string(fp.commands[i]); got != w {
			t.Fatalf("comando %d = %q, se esperaba %q", i, got, w)
		}
	}
}

// TestAbandonedDocumentReleasesTurn comprueba que una factura abierta por una
// estación que desapareció no bloquea a las demás para siempre.
func TestAbandonedDocumentReleasesTurn(t *testing.T) {
	fp := newFakePrinter(t)
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{
		DocumentLockTimeout: 120 * time.Millisecond,
	})

	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("!   1.000     10.00CAFE\r"), Station: "CAJA-01", Session: "s1", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("abrir factura: %v", err)
	}

	start := time.Now()
	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("!   1.000     20.00TE\r"), Station: "CAJA-02", Session: "s2", Timeout: 3 * time.Second,
	}); err != nil {
		t.Fatalf("CAJA-02 debía tomar el turno tras el timeout: %v", err)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("no respetó el turno de la factura abierta (%v)", waited)
	}
	if got := fp.commandCount(); got != 2 {
		t.Fatalf("comandos = %d", got)
	}
	snap := sp.Snapshot()
	if !snap.DocumentOpen || snap.DocumentOwner != "s2" {
		t.Fatalf("el turno debía pasar a s2: %+v", snap)
	}
}

// TestDocumentLockWithoutTimeoutFailsCallers verifica el modo estricto: si el
// turno no se libera automáticamente, el comando retenido vence y NO se imprime.
func TestDocumentLockWithoutTimeoutFailsCallers(t *testing.T) {
	fp := newFakePrinter(t)
	sp, _ := newTestSpooler(t, filepath.Join(t.TempDir(), "db.sqlite"), fp, SpoolerConfig{
		DocumentLockTimeout: 0,
	})

	if _, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("!   1.000     10.00CAFE\r"), Station: "CAJA-01", Session: "s1", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("abrir factura: %v", err)
	}

	_, err := sp.Submit(context.Background(), SubmitRequest{
		Command: []byte("!   1.000     20.00TE\r"), Station: "CAJA-02", Session: "s2", Timeout: 200 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("se esperaba DeadlineExceeded, se obtuvo %v", err)
	}
	if got := fp.commandCount(); got != 1 {
		t.Fatalf("el ítem vencido no debía imprimirse (comandos = %d)", got)
	}
	if snap := sp.Snapshot(); snap.DocumentOwner != "s1" {
		t.Fatalf("el turno sigue retenido por s1: %+v", snap)
	}
}
