package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"spoolerfiscal/internal/frame"
	"spoolerfiscal/internal/server"
	"spoolerfiscal/internal/testutil"
)

// TestLoadTenStations simula diez cajas emitiendo cinco facturas cada una por el
// circuito completo, con latencia artificial en la impresora. Comprueba que el
// correlativo sigue siendo único y que el historial queda completo.
func TestLoadTenStations(t *testing.T) {
	if testing.Short() {
		t.Skip("prueba de carga omitida con -short")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "spooler.db")
	simLn, sim, err := testutil.StartTCPSim(ctx, "127.0.0.1:0", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartTCPSim: %v", err)
	}
	defer func() { _ = simLn.Close() }()
	// ~8 ms por comando: una impresora fiscal real tarda más, pero esto fuerza
	// la contención de la cola sin alargar la prueba.
	sim.SetDelay(8 * time.Millisecond)

	h := startServer(t, dbPath, simLn.Addr().String())

	const stations = 10
	const perStation = 5
	hosts := make([]frame.Stream, stations)
	for i := 0; i < stations; i++ {
		hosts[i] = startAgent(t, ctx, h.rt.Addr(), fmt.Sprintf("CAJA-%02d", i+1))
	}

	start := time.Now()
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
					failures <- fmt.Errorf("%w (respuesta %q)", err, resp)
					return
				}
				numbers <- n
			}
		}(hosts[i])
	}
	wg.Wait()
	close(numbers)
	close(failures)
	elapsed := time.Since(start)

	for err := range failures {
		t.Fatalf("fallo bajo carga: %v", err)
	}

	total := stations * perStation
	got := make([]int64, 0, total)
	for n := range numbers {
		got = append(got, n)
	}
	if len(got) != total {
		t.Fatalf("facturas emitidas = %d, se esperaban %d", len(got), total)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	for i, n := range got {
		if n != int64(i+1) {
			t.Fatalf("correlativo corrupto: %v", got)
		}
	}
	if sim.InvoiceCount() != int64(total) {
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
	if hist.Count != int64(total) || hist.LastNumbers["invoice"] != int64(total) {
		t.Fatalf("historial = %+v", hist)
	}

	t.Logf("%d facturas de %d estaciones en %v (%.1f facturas/s, %.0f ms por factura)",
		total, stations, elapsed.Round(time.Millisecond),
		float64(total)/elapsed.Seconds(), float64(elapsed.Milliseconds())/float64(total))
}
