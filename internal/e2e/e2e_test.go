// Package e2e prueba el circuito completo: Profit Plus (simulado) → COM
// virtual (stream en memoria) → puente → agente HTTP → spooler → impresora
// fiscal emulada por TCP. Verifica transparencia de bytes, correlativo único,
// serialización, detección de duplicados y persistencia tras reinicio.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"spoolerfiscal/internal/agent"
	"spoolerfiscal/internal/comvirt"
	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/fiscal"
	"spoolerfiscal/internal/frame"
	"spoolerfiscal/internal/server"
	"spoolerfiscal/internal/testutil"
)

const testToken = "token-de-prueba-e2e"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type serverHarness struct {
	rt     *server.Runtime
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func startServer(t *testing.T, dbPath, simAddr string) *serverHarness {
	t.Helper()
	return startServerOn(t, dbPath, simAddr, "127.0.0.1:0")
}

func startServerOn(t *testing.T, dbPath, simAddr, listen string) *serverHarness {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.HTTP.Listen = listen
	cfg.Token = testToken
	cfg.Database.Path = dbPath
	cfg.Database.LogCommands = false
	cfg.Printer.Transport = "tcp"
	cfg.Printer.TCPAddress = simAddr
	cfg.Printer.TCPDialTimeoutMS = 2000
	cfg.Printer.ReadGapMS = 10
	cfg.Printer.ResponseTimeoutMS = 4000
	cfg.Queue.DefaultTimeoutMS = 8000
	cfg.Queue.MaxTimeoutMS = 20000
	cfg.Queue.ProbeTimeoutMS = 5000
	cfg.Metrics.IntervalSeconds = 3600
	cfg.Logging = config.Logging{Level: "error"}

	rt, err := server.NewRuntime(cfg, testLogger())
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()

	h := &serverHarness{rt: rt, cancel: cancel, done: done}
	t.Cleanup(h.stop)
	return h
}

func (h *serverHarness) stop() {
	h.once.Do(func() {
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(15 * time.Second):
		}
		_ = h.rt.Close()
	})
}

// startAgent levanta un agente y devuelve el extremo del COM virtual que
// usaría Profit Plus.
func startAgent(t *testing.T, ctx context.Context, addr, station string) frame.Stream {
	t.Helper()
	cfg := config.DefaultAgent()
	cfg.StationID = station
	cfg.Server.Address = "http://" + addr
	cfg.Server.Token = testToken
	cfg.Server.RequestTimeoutMS = 15000
	cfg.Bridge.ReadGapMS = 20
	cfg.Bridge.ResponseTimeoutMS = 15000
	cfg.Bridge.MaxFrameBytes = 4096
	cfg.Bridge.LocalQueueSize = 8
	cfg.Bridge.MaxSendBackoffMS = 200
	cfg.Logging = config.Logging{Level: "error"}

	ag := agent.New(cfg, testLogger())
	if err := ag.Start(ctx); err != nil {
		t.Fatalf("agent.Start: %v", err)
	}
	t.Cleanup(ag.Close)

	host, remote := testutil.Pipe()
	bridge, err := comvirt.NewBridge(remote, ag.Forward, cfg, testLogger())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	go func() { _ = bridge.Run(ctx) }()
	return host
}

func readFrame(t *testing.T, st frame.Stream) []byte {
	t.Helper()
	resp, err := readFrameErr(st)
	if err != nil {
		t.Fatalf("leer respuesta: %v", err)
	}
	return resp
}

func readFrameErr(st frame.Stream) ([]byte, error) {
	return readFrameWithin(st, 12*time.Second)
}

func readFrameWithin(st frame.Stream, total time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()
	return frame.Read(ctx, st, 20*time.Millisecond, total, 8192)
}

// itemLine abre una factura fiscal: en TFHKA la primera línea de ítem abre el
// documento que luego cierra el comando 101.
const itemLine = "!   1.000     10.00CAFE\r\n"

func emitInvoice(t *testing.T, host frame.Stream) []byte {
	t.Helper()
	resp, err := emitInvoiceErr(host)
	if err != nil {
		t.Fatalf("emitir factura: %v", err)
	}
	return resp
}

func emitInvoiceErr(host frame.Stream) ([]byte, error) {
	if err := frame.Write(host, []byte(itemLine)); err != nil {
		return nil, err
	}
	if _, err := readFrameErr(host); err != nil {
		return nil, fmt.Errorf("respuesta al ítem: %w", err)
	}
	if err := frame.Write(host, []byte("101\r\n")); err != nil {
		return nil, err
	}
	return readFrameErr(host)
}

func invoiceNumber(resp []byte) (int64, error) {
	parts := strings.Split(string(fiscal.UnframeSTX(resp)), "|")
	if len(parts) < 2 {
		return 0, fmt.Errorf("respuesta inesperada %q", resp)
	}
	fields := strings.Fields(parts[1])
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Factura") {
		return 0, fmt.Errorf("documento inesperado %q", parts[1])
	}
	return strconv.ParseInt(fields[1], 10, 64)
}

func TestEndToEndInvoice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "spooler.db")
	simLn, sim, err := testutil.StartTCPSim(ctx, "127.0.0.1:0", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartTCPSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()

	h := startServer(t, dbPath, simLn.Addr().String())
	host := startAgent(t, ctx, h.rt.Addr(), "CAJA-01")

	resp := emitInvoice(t, host)
	want := string(fiscal.FrameSTX([]byte("0|Factura 1|Total: 116.00|Impreso:1")))
	if string(resp) != want {
		t.Fatalf("respuesta = %q, se esperaba %q", resp, want)
	}
	if got := sim.InvoiceCount(); got != 1 {
		t.Fatalf("facturas en la impresora = %d", got)
	}

	// El estado de la impresora también viaja por el mismo circuito.
	if err := frame.Write(host, []byte("S2\r\n")); err != nil {
		t.Fatalf("enviar S2: %v", err)
	}
	if resp := readFrame(t, host); !strings.HasPrefix(string(fiscal.UnframeSTX(resp)), "S2") {
		t.Fatalf("respuesta de estado = %q", resp)
	}
}

func TestConcurrentStationsKeepSingleCorrelative(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "spooler.db")
	simLn, sim, err := testutil.StartTCPSim(ctx, "127.0.0.1:0", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartTCPSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()

	h := startServer(t, dbPath, simLn.Addr().String())

	const stations = 3
	const perStation = 3
	hosts := make([]frame.Stream, stations)
	for i := 0; i < stations; i++ {
		hosts[i] = startAgent(t, ctx, h.rt.Addr(), fmt.Sprintf("CAJA-%02d", i+1))
	}

	numbers := make(chan int64, stations*perStation)
	failures := make(chan error, stations*perStation)
	var wg sync.WaitGroup
	for i := 0; i < stations; i++ {
		wg.Add(1)
		go func(host frame.Stream) {
			defer wg.Done()
			for j := 0; j < perStation; j++ {
				resp, err := emitInvoiceErr(host)
				if err != nil {
					failures <- err
					return
				}
				n, err := invoiceNumber(resp)
				if err != nil {
					failures <- err
					return
				}
				numbers <- n
			}
		}(hosts[i])
	}
	wg.Wait()
	close(numbers)
	close(failures)

	for err := range failures {
		t.Fatalf("fallo de estación: %v", err)
	}
	var got []int64
	for n := range numbers {
		got = append(got, n)
	}
	if len(got) != stations*perStation {
		t.Fatalf("facturas = %d", len(got))
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	for i, n := range got {
		if n != int64(i+1) {
			t.Fatalf("correlativo corrupto: %v", got)
		}
	}
	if sim.InvoiceCount() != int64(stations*perStation) {
		t.Fatalf("la impresora emitió %d", sim.InvoiceCount())
	}

	h.stop()
	store, err := server.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	hist, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if hist.Count != int64(stations*perStation) {
		t.Fatalf("documentos persistidos = %d", hist.Count)
	}
	if hist.LastNumbers["invoice"] != int64(stations*perStation) {
		t.Fatalf("última factura = %d", hist.LastNumbers["invoice"])
	}
}

func TestRestartPreservesCorrelativeAndHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "spooler.db")
	simLn, sim, err := testutil.StartTCPSim(ctx, "127.0.0.1:0", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartTCPSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()

	h1 := startServer(t, dbPath, simLn.Addr().String())
	host1 := startAgent(t, ctx, h1.rt.Addr(), "CAJA-01")
	if n, err := invoiceNumber(emitInvoice(t, host1)); err != nil || n != 1 {
		t.Fatalf("primera factura = %d, err=%v", n, err)
	}
	h1.stop()

	// Reinicio del servicio sobre la misma base y la misma impresora.
	h2 := startServer(t, dbPath, simLn.Addr().String())
	host2 := startAgent(t, ctx, h2.rt.Addr(), "CAJA-01")
	if n, err := invoiceNumber(emitInvoice(t, host2)); err != nil || n != 2 {
		t.Fatalf("segunda factura = %d, err=%v", n, err)
	}
	h2.stop()

	if got := sim.InvoiceCount(); got != 2 {
		t.Fatalf("facturas impresas = %d", got)
	}

	store, err := server.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	hist, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if hist.Count != 2 || hist.LastNumbers["invoice"] != 2 {
		t.Fatalf("historial tras reinicio = %+v", hist)
	}
}

func TestAuthTokenIsRequired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	simLn, _, err := testutil.StartTCPSim(ctx, "127.0.0.1:0", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartTCPSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()
	h := startServer(t, filepath.Join(t.TempDir(), "spooler.db"), simLn.Addr().String())
	base := "http://" + h.rt.Addr()

	// health es público y reporta la versión del contrato.
	res, err := http.Get(base + "/v1/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	var health struct {
		ProtocolVersion uint32 `json:"protocol_version"`
	}
	_ = json.NewDecoder(res.Body).Decode(&health)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", res.StatusCode)
	}
	if health.ProtocolVersion != 1 {
		t.Fatalf("versión de protocolo = %d", health.ProtocolVersion)
	}

	// status sin token: 401.
	res, err = http.Get(base + "/v1/status")
	if err != nil {
		t.Fatalf("status sin token: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sin token se esperaba 401, se obtuvo %d", res.StatusCode)
	}

	// con el token correcto: 200.
	req, err := http.NewRequest(http.MethodGet, base+"/v1/status", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status con token: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("con token se esperaba 200, se obtuvo %d", res.StatusCode)
	}

	// token equivocado: 401.
	req, err = http.NewRequest(http.MethodGet, base+"/v1/status", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token-equivocado")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status con token equivocado: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token inválido aceptado: %d", res.StatusCode)
	}
}

// TestUnavailableServerDoesNotBreakNextCommand comprueba la reconexión: con el
// servidor caído la trama falla, y al volver el servicio la siguiente trama
// funciona sin reiniciar el agente.
func TestUnavailableServerDoesNotBreakNextCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	simLn, sim, err := testutil.StartTCPSim(ctx, "127.0.0.1:0", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartTCPSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()

	// Reservamos un puerto libre y lo liberamos para que el agente apunte ahí
	// antes de que exista el servidor.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reservar puerto: %v", err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("liberar puerto: %v", err)
	}

	agentCfg := config.DefaultAgent()
	agentCfg.StationID = "CAJA-01"
	agentCfg.Server.Address = "http://" + addr
	agentCfg.Server.Token = testToken
	agentCfg.Server.RequestTimeoutMS = 900
	agentCfg.Bridge.ReadGapMS = 20
	agentCfg.Bridge.ResponseTimeoutMS = 5000
	agentCfg.Bridge.MaxFrameBytes = 4096
	agentCfg.Bridge.MaxSendBackoffMS = 100
	agentCfg.Logging = config.Logging{Level: "error"}

	ag := agent.New(agentCfg, testLogger())
	if err := ag.Start(ctx); err != nil {
		t.Fatalf("agent.Start: %v", err)
	}
	t.Cleanup(ag.Close)

	host, remote := testutil.Pipe()
	bridge, err := comvirt.NewBridge(remote, ag.Forward, agentCfg, testLogger())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	go func() { _ = bridge.Run(ctx) }()

	// Sin servidor: el agente espera hasta agotar su timeout y no responde.
	if err := frame.Write(host, []byte("101\r\n")); err != nil {
		t.Fatalf("enviar: %v", err)
	}
	if _, err := readFrameWithin(host, 3*time.Second); !errors.Is(err, frame.ErrTimeout) {
		t.Fatalf("con el servidor caído se esperaba timeout, se obtuvo %v", err)
	}

	// Al arrancar el servidor en esa misma dirección, el agente se reconecta
	// solo y la siguiente factura se emite normalmente.
	_ = startServerOn(t, filepath.Join(t.TempDir(), "spooler.db"), simLn.Addr().String(), addr)

	resp := emitInvoice(t, host)
	if n, err := invoiceNumber(resp); err != nil || n != 1 {
		t.Fatalf("factura tras reconexión = %v (%q)", n, resp)
	}
	if sim.InvoiceCount() != 1 {
		t.Fatalf("facturas impresas = %d", sim.InvoiceCount())
	}
}

// TestAgainstExternalPrinter valida el circuito contra una impresora fiscal
// expuesta por TCP, por ejemplo el simulador del proyecto SimuladorFiscalGO:
//
//	simuladorfiscal.exe serve --model tfhka --addr 127.0.0.1:2011
//	$env:SPOOLER_E2E_PRINTER_ADDR = "127.0.0.1:2011"
//	go test ./internal/e2e -run TestAgainstExternalPrinter -v
func TestAgainstExternalPrinter(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("SPOOLER_E2E_PRINTER_ADDR"))
	if addr == "" {
		t.Skip("SPOOLER_E2E_PRINTER_ADDR no está definido")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := startServerOn(t, filepath.Join(t.TempDir(), "spooler.db"), addr, "127.0.0.1:0")
	host := startAgent(t, ctx, h.rt.Addr(), "CAJA-EXT")

	invoice := emitInvoice(t, host)
	if !strings.Contains(string(invoice), "Factura") {
		t.Fatalf("la impresora externa no reportó factura: %q", invoice)
	}

	if err := frame.Write(host, []byte("S1\r\n")); err != nil {
		t.Fatalf("enviar S1: %v", err)
	}
	info := readFrame(t, host)
	if !strings.Contains(string(info), "|") {
		t.Fatalf("respuesta S1 inesperada: %q", info)
	}

	t.Logf("factura: %q", invoice)
	t.Logf("S1:      %q", info)
}
