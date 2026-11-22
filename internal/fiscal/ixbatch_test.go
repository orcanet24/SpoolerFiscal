package fiscal

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"spoolerfiscal/internal/frame"
)

func newTestIxBatch(t *testing.T, brand string, responses ...string) (*IxBatch, *scriptStream) {
	t.Helper()
	stream := &scriptStream{}
	for _, r := range responses {
		stream.reads = append(stream.reads, []byte(r))
	}
	p := newIxBatch(brand, Options{
		NewTransport:     func() (frame.Stream, error) { return stream, nil },
		ReadGap:          5 * time.Millisecond,
		ResponseTimeout:  time.Second,
		MaxResponseBytes: 4096,
		StatusProbe:      "S2", // la sonda por omisión del YAML es de TFHKA
		CommandEnding:    "\r\n",
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return p, stream
}

func TestIxBatchStepClassifiesCommands(t *testing.T) {
	p := newIxBatch(BrandHasar, Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	cases := []struct {
		cmd  string
		want Step
	}{
		{"@OpenFiscalReceipt", StepOpenDocument},
		{"@OpenFiscalReceipt\r\n", StepOpenDocument},
		{"@opencreditnote", StepOpenDocument},
		{"@OpenDebitNote", StepOpenDocument},
		{"@OpenNonFiscalDoc", StepOpenDocument},
		{"@PrintLineItem|CAFE|1.000|10.00|general", StepOpenDocument},
		{"@PrintLine|CAFE|1|10.00|general", StepOpenDocument},
		{"@Close", StepCloseDocument},
		{"@CloseFiscalReceipt", StepCloseDocument},
		{"@CloseCashReceipt", StepCloseDocument},
		{"@Cancel", StepCloseDocument},
		{"@Status", StepNeutral},
		{"@PrintXReport", StepNeutral},
		{"@PrintZReport", StepNeutral},
		{"@CustomerData|J-1|Cliente", StepNeutral},
		{"!   1.000     10.00CAFE", StepNeutral}, // comando TFHKA en driver IxBatch
		{"S2", StepNeutral},
		{"", StepNeutral},
		{"   ", StepNeutral},
	}
	for _, tc := range cases {
		if got := p.Step([]byte(tc.cmd)); got != tc.want {
			t.Errorf("Step(%q) = %v, se esperaba %v", tc.cmd, got, tc.want)
		}
	}
}

// La sonda y el terminador de la configuración son de TFHKA; el driver los
// corrige para IxBatch (y lo registra).
func TestIxBatchNormalizesProbeAndTerminator(t *testing.T) {
	p := newIxBatch(BrandBixolon, Options{
		StatusProbe:   "S2",
		CommandEnding: "\r",
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if p.opts.StatusProbe != CmdIxStatus {
		t.Fatalf("sonda = %q, se esperaba %q", p.opts.StatusProbe, CmdIxStatus)
	}
	if p.opts.CommandEnding != "\r\n" {
		t.Fatalf("terminador = %q", p.opts.CommandEnding)
	}

	// Un terminador que ya trae LF se respeta.
	p = newIxBatch(BrandHasar, Options{
		CommandEnding: "\n",
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if p.opts.CommandEnding != "\n" {
		t.Fatalf("terminador = %q", p.opts.CommandEnding)
	}
}

func TestNormalizeCommandTerminator(t *testing.T) {
	cases := []struct{ driver, in, want string }{
		{BrandHasar, "", "\r\n"},
		{BrandBixolon, "\r", "\r\n"},
		{BrandEpson, "\n", "\n"},
		{BrandEpson, "\r\n", "\r\n"},
		{BrandTFHKA, "\r", "\r"},
		{BrandTFHKA, "", ""},
	}
	for _, tc := range cases {
		if got := NormalizeCommandTerminator(tc.driver, tc.in); got != tc.want {
			t.Errorf("NormalizeCommandTerminator(%q, %q) = %q, se esperaba %q",
				tc.driver, tc.in, got, tc.want)
		}
	}
}

func TestIxBatchDescribeCloseResponse(t *testing.T) {
	p, _ := newTestIxBatch(t, BrandHasar)
	desc := p.Describe([]byte("@Close"), []byte("@Response|OK|0|Factura 12 cerrada|Total: 116.00|Impreso:3"))
	if desc.Failed {
		t.Fatalf("no debió fallar: %+v", desc)
	}
	if desc.Document == nil {
		t.Fatal("no detectó el documento")
	}
	if desc.Document.Type != DocInvoice || desc.Document.Number != 12 {
		t.Fatalf("documento = %+v", desc.Document)
	}
	if desc.Document.Total != "116.00" {
		t.Fatalf("total = %q", desc.Document.Total)
	}
}

func TestIxBatchDescribeNotesAndNonFiscal(t *testing.T) {
	p, _ := newTestIxBatch(t, BrandHasar)
	cases := []struct {
		resp string
		typ  string
		num  int64
	}{
		{"@Response|OK|0|Nota de Credito 3 cerrada|Total: 10.00|Impreso:1", DocCredit, 3},
		{"@Response|OK|0|Nota de Debito 4 cerrada|Total: 5.00|Impreso:1", DocDebit, 4},
		{"@Response|OK|0|Documento 7 cerrada|Total: 0.00|Impreso:1", DocNonFisc, 7},
		{"@Response|OK|0|DNF cerrado", DocNonFisc, 0},
	}
	for _, tc := range cases {
		desc := p.Describe([]byte("@Close"), []byte(tc.resp))
		if desc.Document == nil {
			t.Fatalf("%q: sin documento", tc.resp)
		}
		if desc.Document.Type != tc.typ || desc.Document.Number != tc.num {
			t.Fatalf("%q: documento = %+v", tc.resp, desc.Document)
		}
	}
}

// Epson antepone "0xESTADO|0xERROR|" a cada respuesta.
func TestIxBatchDescribeEpsonEnvelope(t *testing.T) {
	p, _ := newTestIxBatch(t, BrandEpson)
	desc := p.Describe([]byte("@Close"),
		[]byte("0x05|0x00|@Response|OK|0|Factura 1 cerrada|Total: 20.00|Impreso:1"))
	if desc.Failed {
		t.Fatalf("no debió fallar: %+v", desc)
	}
	if desc.Status.StatusByte != 0x05 || !desc.Status.DocumentOpen {
		t.Fatalf("estado = %+v", desc.Status)
	}
	if desc.Document == nil || desc.Document.Number != 1 {
		t.Fatalf("documento = %+v", desc.Document)
	}

	// Envoltorio con fin de papel: el guardián debe verlo.
	desc = p.Describe([]byte("@Status"),
		[]byte("0x05|0x01|@Response|OK|0|Status: 0x05|Error: 0x01|Doc: ABIERTO"))
	if !desc.Status.PaperOut {
		t.Fatalf("papel agotado no detectado: %+v", desc.Status)
	}
}

func TestIxBatchDescribeStatusBlock(t *testing.T) {
	p, _ := newTestIxBatch(t, BrandHasar)
	desc := p.Describe([]byte("@Status"),
		[]byte("@Response|OK|0|Status: 0x05|Error: 0x00|Doc: ABIERTO|Ventas: 100.00"))
	st := desc.Status
	if !st.Online || st.StatusByte != 0x05 || st.ErrorByte != 0x00 {
		t.Fatalf("estado = %+v", st)
	}
	if !st.DocumentOpen {
		t.Fatal("documento abierto no detectado")
	}
	if st.StatusText == "" || st.ErrorText != "Sin error" {
		t.Fatalf("textos = %q / %q", st.StatusText, st.ErrorText)
	}

	// CERRADO debe limpiar el flag de documento abierto.
	desc = p.Describe([]byte("@Status"),
		[]byte("@Response|OK|0|Status: 0x04|Error: 0x00|Doc: CERRADO|Ventas: 0.00"))
	if desc.Status.DocumentOpen {
		t.Fatalf("documento cerrado marcado como abierto: %+v", desc.Status)
	}
}

func TestIxBatchDescribeErrors(t *testing.T) {
	p, _ := newTestIxBatch(t, BrandHasar)

	desc := p.Describe([]byte("@Comando"), []byte("@Response|Error|0x50|Comando desconocido"))
	if !desc.Failed || desc.ErrorCode != 0x50 {
		t.Fatalf("descripción = %+v", desc)
	}
	if desc.ErrorText != "Comando desconocido" {
		t.Fatalf("texto = %q", desc.ErrorText)
	}

	// Código decimal (el protocolo de referencia mezcla decimal y 0xNN).
	desc = p.Describe([]byte("@PrintLine"), []byte("@Response|Error|2|Invalid params"))
	if !desc.Failed || desc.ErrorCode != 2 {
		t.Fatalf("descripción = %+v", desc)
	}

	// Fin de papel: además de error, el guardián lo ve como PAPER_OUT.
	desc = p.Describe([]byte("@Status"), []byte("@Response|ERROR|0x01|Fin de papel"))
	if !desc.Failed || !desc.Status.PaperOut {
		t.Fatalf("descripción = %+v", desc)
	}
}

// Una respuesta correcta sin bloque de estado no debe arrastrar flags viejos.
func TestIxBatchSuccessClearsStaleStatus(t *testing.T) {
	p, _ := newTestIxBatch(t, BrandHasar)
	p.Describe([]byte("@Status"), []byte("@Response|OK|0|Status: 0x0A|Error: 0x6C|Doc: CERRADO"))
	if !p.cached().MemoryFull {
		t.Fatal("memoria llena no detectada")
	}
	desc := p.Describe([]byte("@Cancel"), []byte("@Response|OK|0|Cancelado"))
	if desc.Status.MemoryFull || desc.Status.PaperOut {
		t.Fatalf("flags viejos arrastrados: %+v", desc.Status)
	}
}

// La descripción de un ítem la escribe el usuario: no debe poder fabricar un
// documento ni un estado de la impresora.
func TestIxBatchItemTextCannotFakeDocumentOrStatus(t *testing.T) {
	p, _ := newTestIxBatch(t, BrandHasar)

	desc := p.Describe([]byte("@PrintLineItem"),
		[]byte("@Response|OK|0|Factura 5|1.000|10.00|10.00"))
	if desc.Document != nil {
		t.Fatalf("una descripción se interpretó como documento: %+v", desc.Document)
	}

	desc = p.Describe([]byte("@PrintLineItem"),
		[]byte("@Response|OK|0|Error: 0x01|1.000|10.00|10.00"))
	if desc.Status.PaperOut || desc.Failed {
		t.Fatalf("una descripción falsificó el estado: %+v", desc.Status)
	}

	desc = p.Describe([]byte("@PrintLineItem"),
		[]byte("@Response|OK|0|Status: 0x0A|1.000|10.00|10.00"))
	if desc.Status.MemoryFull {
		t.Fatalf("una descripción falsificó la memoria fiscal: %+v", desc.Status)
	}
}

func TestIxBatchGetStatusSendsProbe(t *testing.T) {
	p, stream := newTestIxBatch(t, BrandHasar,
		"@Response|OK|0|Status: 0x04|Error: 0x00|Doc: CERRADO|Ventas: 0.00")
	if err := p.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = p.Close() }()

	st, err := p.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.StatusByte != 0x04 || st.DocumentOpen {
		t.Fatalf("estado = %+v", st)
	}
	if got := stream.written.String(); got != "@Status\r\n" {
		t.Fatalf("comando enviado = %q", got)
	}
}

func TestIxBatchGetInfoProbesAndIdentifies(t *testing.T) {
	p, _ := newTestIxBatch(t, BrandEpson,
		"0x04|0x00|@Response|OK|0|Status: 0x04|Error: 0x00|Doc: CERRADO|Ventas: 0.00")
	if err := p.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = p.Close() }()

	info, err := p.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.Brand != BrandEpson || info.Model != "TM2000" {
		t.Fatalf("info = %+v", info)
	}
}

func TestIxBatchDriverNamesAndRegistry(t *testing.T) {
	for _, brand := range []string{BrandHasar, BrandBixolon, BrandEpson} {
		p, err := New(brand, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatalf("New(%q): %v", brand, err)
		}
		if p.Driver() != brand {
			t.Fatalf("Driver() = %q, se esperaba %q", p.Driver(), brand)
		}
	}
	drivers := strings.Join(Drivers(), ",")
	for _, want := range []string{BrandTFHKA, BrandHasar, BrandBixolon, BrandEpson} {
		if !strings.Contains(drivers, want) {
			t.Fatalf("registro incompleto: %q", drivers)
		}
	}
}
