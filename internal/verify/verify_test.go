package verify

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/testutil"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testPrinterCfg(addr string) config.PrinterCfg {
	return config.PrinterCfg{
		Driver: "tfhka", Transport: "tcp", TCPAddress: addr,
		TCPDialTimeoutMS: 1000, ReadGapMS: 10, ResponseTimeoutMS: 2000,
		MaxResponseBytes: 4096, StatusProbe: "S2", CommandTerminator: "\r\n",
	}
}

func TestRunReadOnlyPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, sim, err := testutil.StartTCPSim(ctx, "127.0.0.1:0", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartTCPSim: %v", err)
	}
	defer func() { _ = ln.Close() }()

	rep, err := Run(ctx, testPrinterCfg(ln.Addr().String()), testLogger(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.OK {
		t.Fatalf("informe = %+v", rep)
	}
	if len(rep.Checks) < 5 {
		t.Fatalf("faltan comprobaciones: %+v", rep.Checks)
	}
	if rep.Info == nil || rep.Info.Serial == "" {
		t.Fatalf("sin identificación: %+v", rep.Info)
	}
	if rep.Status == nil || !rep.Status.Online {
		t.Fatalf("sin estado: %+v", rep.Status)
	}
	if sim.InvoiceCount() != 0 {
		t.Fatalf("no debía imprimir documentos fiscales")
	}
}

func TestRunPrintsXAndNonFiscal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, sim, err := testutil.StartTCPSim(ctx, "127.0.0.1:0", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("StartTCPSim: %v", err)
	}
	defer func() { _ = ln.Close() }()

	rep, err := Run(ctx, testPrinterCfg(ln.Addr().String()), testLogger(),
		Options{PrintX: true, PrintNonFiscal: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.OK {
		t.Fatalf("informe = %+v", rep)
	}
	joined := strings.Join(sim.Commands(), "\n")
	for _, want := range []string{"U0X", "80$", "80!", "80*", "81"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("no se envió %q; comandos = %q", want, joined)
		}
	}
	if sim.InvoiceCount() != 0 {
		t.Fatalf("las pruebas no emiten facturas fiscales")
	}
}

func TestRunFailsWhenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rep, err := Run(ctx, testPrinterCfg("127.0.0.1:1"), testLogger(), Options{Timeout: 300 * time.Millisecond})
	if err == nil {
		t.Fatal("se esperaba error de conexión")
	}
	if rep.OK {
		t.Fatal("el informe no puede ser OK sin impresora")
	}
	if rep.Error == "" {
		t.Fatal("el informe debe explicar el error")
	}
}

func TestCompare(t *testing.T) {
	base := Report{Driver: "tfhka", Info: &Info{Serial: "ML1", RIF: "J-1", LastInvoice: 100}}
	same := Report{Driver: "tfhka", Info: &Info{Serial: "ML1", RIF: "j-1", LastInvoice: 120}}
	if p := Compare(base, same); len(p) != 0 {
		t.Fatalf("sin diferencias: %v", p)
	}
	other := Report{Driver: "tfhka", Info: &Info{Serial: "ML2", RIF: "J-2", LastInvoice: 90}}
	problems := Compare(base, other)
	if len(problems) != 3 {
		t.Fatalf("problemas = %v", problems)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"serial", "RIF", "correlativo retrocedió"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("falta %q en %q", want, joined)
		}
	}
	// Sin datos de la base no se inventan diferencias.
	if p := Compare(Report{}, same); len(p) != 0 {
		t.Fatalf("base vacía: %v", p)
	}
}

func TestTransportLabel(t *testing.T) {
	cfg := testPrinterCfg("127.0.0.1:2011")
	if got := transportLabel(cfg); !strings.Contains(got, "tcp 127.0.0.1:2011") {
		t.Fatalf("etiqueta = %q", got)
	}
	cfg.Transport = "serial"
	cfg.Port.Name = "COM1"
	cfg.Port.BaudRate = 9600
	if got := transportLabel(cfg); !strings.Contains(got, "serial COM1 @ 9600") {
		t.Fatalf("etiqueta = %q", got)
	}
}
