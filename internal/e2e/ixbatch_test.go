package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/frame"
	"spoolerfiscal/internal/server"
	"spoolerfiscal/internal/testutil"
)

// startIxBatchServer levanta el servidor con un driver IxBatch. Deja la sonda
// de estado en el valor por omisión del YAML (S2, de TFHKA) a propósito: el
// driver debe corregirla a @Status.
func startIxBatchServer(t *testing.T, dbPath, simAddr, driver string) *serverHarness {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.HTTP.Listen = "127.0.0.1:0"
	cfg.Token = testToken
	cfg.Database.Path = dbPath
	cfg.Database.LogCommands = false
	cfg.Printer.Driver = driver
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
		t.Fatalf("NewRuntime(%s): %v", driver, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()

	h := &serverHarness{rt: rt, cancel: cancel, done: done}
	t.Cleanup(h.stop)
	return h
}

const ixItem = "@PrintLineItem|CAFE|1.000|10.00|general"

func ixSend(t *testing.T, host frame.Stream, cmd string) string {
	t.Helper()
	if err := frame.Write(host, []byte(cmd+"\r\n")); err != nil {
		t.Fatalf("enviar %q: %v", cmd, err)
	}
	return string(readFrame(t, host))
}

var ixInvoiceRe = regexp.MustCompile(`Factura (\d+)`)

// ixInvoice emite una factura con el protocolo nativo IxBatch y devuelve la
// respuesta del cierre y el número asignado por la impresora.
func ixInvoice(t *testing.T, host frame.Stream) (string, int64) {
	t.Helper()
	if resp := ixSend(t, host, "@OpenFiscalReceipt"); !strings.Contains(resp, "OK") {
		t.Fatalf("apertura: %q", resp)
	}
	if resp := ixSend(t, host, ixItem); !strings.Contains(resp, "OK") {
		t.Fatalf("ítem: %q", resp)
	}
	resp := ixSend(t, host, "@Close")
	m := ixInvoiceRe.FindStringSubmatch(resp)
	if m == nil {
		t.Fatalf("cierre sin número de factura: %q", resp)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("número de factura %q: %v", m[1], err)
	}
	return resp, n
}

// El circuito completo con Hasar: el agente transporta los bytes @... tal
// cual, el spooler los clasifica, la impresora emulada responde y el número
// que reporta el cierre queda auditado.
func TestIxBatchInvoiceEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "spooler.db")
	simLn, sim, err := testutil.StartTCPIxSim(ctx, "127.0.0.1:0", 10*time.Millisecond, "hasar")
	if err != nil {
		t.Fatalf("StartTCPIxSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()

	h := startIxBatchServer(t, dbPath, simLn.Addr().String(), "hasar")
	host := startAgent(t, ctx, h.rt.Addr(), "CAJA-HASAR")

	resp, n := ixInvoice(t, host)
	if n != 1 {
		t.Fatalf("factura = %d (%q)", n, resp)
	}
	if !strings.Contains(resp, "Total: 10.00") {
		t.Fatalf("total en la respuesta: %q", resp)
	}
	if got := sim.InvoiceCount(); got != 1 {
		t.Fatalf("facturas en la impresora = %d", got)
	}

	// El estado viaja por el mismo circuito con la sonda del protocolo.
	if resp := ixSend(t, host, "@Status"); !strings.Contains(resp, "Doc: CERRADO") {
		t.Fatalf("estado = %q", resp)
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
	if hist.Count != 1 || hist.LastNumbers["invoice"] != 1 {
		t.Fatalf("historial = %+v", hist)
	}
}

// Epson envuelve cada respuesta con "0xESTADO|0xERROR|": el driver debe
// interpretar el sobre y aun así registrar el documento.
func TestIxBatchEpsonEnvelopeEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	simLn, sim, err := testutil.StartTCPIxSim(ctx, "127.0.0.1:0", 10*time.Millisecond, "epson")
	if err != nil {
		t.Fatalf("StartTCPIxSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()

	h := startIxBatchServer(t, filepath.Join(t.TempDir(), "spooler.db"), simLn.Addr().String(), "epson")
	host := startAgent(t, ctx, h.rt.Addr(), "CAJA-EPSON")

	resp, n := ixInvoice(t, host)
	if n != 1 {
		t.Fatalf("factura = %d (%q)", n, resp)
	}
	if !strings.HasPrefix(resp, "0x04|0x00|@Response|OK|0|") {
		t.Fatalf("sobre de Epson inesperado: %q", resp)
	}
	if got := sim.InvoiceCount(); got != 1 {
		t.Fatalf("facturas en la impresora = %d", got)
	}
}

// Tres estaciones IxBatch emitiendo a la vez: el turno exclusivo debe mantener
// el correlativo único, igual que con TFHKA.
func TestIxBatchConcurrentStationsKeepSingleCorrelative(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	simLn, sim, err := testutil.StartTCPIxSim(ctx, "127.0.0.1:0", 10*time.Millisecond, "hasar")
	if err != nil {
		t.Fatalf("StartTCPIxSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()

	h := startIxBatchServer(t, filepath.Join(t.TempDir(), "spooler.db"), simLn.Addr().String(), "bixolon")
	// El driver es bixolon pero el emulador responde como hasar: sólo cambia
	// el modelo declarado y ambos comparten protocolo y respuestas.

	const stations = 3
	const perStation = 2
	hosts := make([]frame.Stream, stations)
	for i := 0; i < stations; i++ {
		hosts[i] = startAgent(t, ctx, h.rt.Addr(), fmt.Sprintf("CAJA-IX-%02d", i+1))
	}

	numbers := make(chan int64, stations*perStation)
	failures := make(chan error, stations*perStation)
	var wg sync.WaitGroup
	for i := 0; i < stations; i++ {
		wg.Add(1)
		go func(host frame.Stream) {
			defer wg.Done()
			for j := 0; j < perStation; j++ {
				if err := frame.Write(host, []byte("@OpenFiscalReceipt\r\n")); err != nil {
					failures <- err
					return
				}
				if _, err := readFrameErr(host); err != nil {
					failures <- err
					return
				}
				if err := frame.Write(host, []byte(ixItem+"\r\n")); err != nil {
					failures <- err
					return
				}
				if _, err := readFrameErr(host); err != nil {
					failures <- err
					return
				}
				if err := frame.Write(host, []byte("@Close\r\n")); err != nil {
					failures <- err
					return
				}
				resp, err := readFrameErr(host)
				if err != nil {
					failures <- err
					return
				}
				m := ixInvoiceRe.FindStringSubmatch(string(resp))
				if m == nil {
					failures <- fmt.Errorf("cierre sin número: %q", resp)
					return
				}
				n, err := strconv.ParseInt(m[1], 10, 64)
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
}
