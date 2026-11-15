package fiscal

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"spoolerfiscal/internal/frame"
)

func TestTFHKAStepClassifiesCommands(t *testing.T) {
	p := newTFHKA(Options{})

	cases := []struct {
		cmd  string
		want Step
	}{
		{"!   1.000     10.00CAFE", StepOpenDocument},   // ítem gravado
		{"\"  1.000      0.00BOLSA", StepOpenDocument},  // ítem exento
		{"!   1.000     10.00CAFE\r", StepOpenDocument}, // con terminador
		{"101", StepCloseDocument},                      // cierre de factura
		{"101\r", StepCloseDocument},
		{"81", StepCloseDocument}, // cierre de no fiscal
		{" S1", StepNeutral},      // consulta con espacios de más
		{"S1", StepNeutral},
		{"S2", StepNeutral},
		{"U730", StepNeutral},  // consulta de memoria
		{" 9901", StepNeutral}, // reporte X
		{"I0", StepNeutral},    // comando no fiscal suelto
		{"", StepNeutral},      // vacío
		{"   ", StepNeutral},   // sólo espacios
	}
	for _, tc := range cases {
		if got := p.Step([]byte(tc.cmd)); got != tc.want {
			t.Errorf("Step(%q) = %v, se esperaba %v", tc.cmd, got, tc.want)
		}
	}
}

type scriptStream struct {
	mu         sync.Mutex
	reads      [][]byte
	written    bytes.Buffer
	closed     bool
	lastServed time.Time
}

func (s *scriptStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if len(s.reads) == 0 {
		s.mu.Unlock()
		time.Sleep(time.Millisecond)
		return 0, nil
	}
	// Entre respuestas hay silencio: así cada una cierra su propia trama.
	if !s.lastServed.IsZero() {
		if idle := 20*time.Millisecond - time.Since(s.lastServed); idle > 0 {
			s.mu.Unlock()
			time.Sleep(idle)
			return 0, nil
		}
	}
	chunk := s.reads[0]
	n := copy(p, chunk)
	if n == len(chunk) {
		s.reads = s.reads[1:]
	} else {
		s.reads[0] = chunk[n:]
	}
	s.lastServed = time.Now()
	s.mu.Unlock()
	return n, nil
}

func (s *scriptStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written.Write(p)
}

func (s *scriptStream) Close() error                       { s.closed = true; return nil }
func (s *scriptStream) SetReadTimeout(time.Duration) error { return nil }

func newTestPrinter(t *testing.T, responses ...string) (*TFHKA, *scriptStream) {
	t.Helper()
	stream := &scriptStream{}
	for _, r := range responses {
		stream.reads = append(stream.reads, []byte(r))
	}
	p, err := New("tfhka", Options{
		NewTransport:     func() (frame.Stream, error) { return stream, nil },
		ReadGap:          5 * time.Millisecond,
		ResponseTimeout:  time.Second,
		MaxResponseBytes: 4096,
		StatusProbe:      "S2",
		CommandEnding:    "\r",
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*TFHKA), stream
}

func TestDescribeDetectsInvoice(t *testing.T) {
	p, _ := newTestPrinter(t)
	desc := p.Describe([]byte("101"), []byte("0|Factura 12|Total: 116.00|Impreso:1"))
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

func TestDescribeDetectsCreditAndDebitNotes(t *testing.T) {
	p, _ := newTestPrinter(t)
	if d := p.Describe(nil, []byte("0|Nota de Credito 3|Total: 10.00")); d.Document == nil || d.Document.Type != DocCredit {
		t.Fatalf("nota de crédito no detectada: %+v", d.Document)
	}
	if d := p.Describe(nil, []byte("0|Nota de Debito 4|Total: 10.00")); d.Document == nil || d.Document.Type != DocDebit {
		t.Fatalf("nota de débito no detectada: %+v", d.Document)
	}
	if d := p.Describe(nil, []byte("0|Doc No Fiscal 5|Total: 0.00")); d.Document == nil || d.Document.Type != DocNonFisc {
		t.Fatalf("documento no fiscal no detectado: %+v", d.Document)
	}
}

func TestDescribeParsesErrorByte(t *testing.T) {
	p, _ := newTestPrinter(t)
	desc := p.Describe([]byte("101"), []byte("ERROR|0x01|Fin de papel"))
	if !desc.Failed {
		t.Fatal("debía marcar error")
	}
	if desc.ErrorCode != 0x01 {
		t.Fatalf("código = 0x%02X", desc.ErrorCode)
	}
	if !desc.Status.PaperOut {
		t.Fatal("debía marcar fin de papel")
	}
	if desc.Document != nil {
		t.Fatal("un error no puede reportar documento")
	}
}

func TestDescribeParsesStatusBlock(t *testing.T) {
	p, _ := newTestPrinter(t)
	desc := p.Describe([]byte("101"), []byte("0x0A|0x00|0|Factura 9|Total: 1.00"))
	if desc.Status.StatusByte != 0x0A {
		t.Fatalf("status = 0x%02X", desc.Status.StatusByte)
	}
	if !desc.Status.MemoryFull {
		t.Fatal("debía marcar memoria fiscal llena")
	}
	if desc.Document == nil || desc.Document.Number != 9 {
		t.Fatalf("documento = %+v", desc.Document)
	}
}

func TestDescribeClearsStaleFlagsOnCleanResponse(t *testing.T) {
	p, _ := newTestPrinter(t)
	first := p.Describe(nil, []byte("0x04|0x01|0|OK"))
	if !first.Status.PaperOut {
		t.Fatal("debía marcar fin de papel")
	}
	second := p.Describe(nil, []byte("0|OK"))
	if second.Status.PaperOut || second.Status.ErrorByte != 0 {
		t.Fatalf("flags viejos arrastrados: %+v", second.Status)
	}
}

func TestGetStatusProbesPrinter(t *testing.T) {
	p, stream := newTestPrinter(t, "0|0|1|0.00|0.00|0.00|0\r\n")
	if err := p.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = p.Close() }()

	st, err := p.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !st.Online {
		t.Fatal("debía reportar online")
	}
	stream.mu.Lock()
	written := stream.written.String()
	stream.mu.Unlock()
	if written != "S2\r" {
		t.Fatalf("comando de sonda = %q", written)
	}
}

func TestGetInfoParsesS1AndS5(t *testing.T) {
	p, stream := newTestPrinter(t,
		"0|SIMTFHKA01|J-12345678-9|7|1|42|3|2|1|1160.00|2026-11-14T10:00:00\r\n",
		"SIMTFHKA01|J-12345678-9|1|4.00|3.60|7\r\n",
		"SIMTFHKA01|J-12345678-9|1|4.00|3.40|7\r\n",
	)
	if err := p.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = p.Close() }()

	info, err := p.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.Serial != "SIMTFHKA01" || info.RIF != "J-12345678-9" {
		t.Fatalf("identificación = %+v", info)
	}
	if info.LastInvoice != 42 || info.LastCredit != 3 || info.LastDebit != 2 {
		t.Fatalf("correlativos reportados = %+v", info)
	}
	if info.MemoryTotalMB != 4.0 || info.MemoryFreeMB != 3.60 {
		t.Fatalf("memoria fiscal = %+v", info)
	}

	// La identificación está cacheada (S1 una sola vez); la memoria fiscal se
	// consulta siempre (S5 en cada llamada) para que el monitoreo sea real.
	info2, err := p.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo segunda llamada: %v", err)
	}
	if info2.MemoryFreeMB != 3.40 {
		t.Fatalf("la memoria fiscal debía refrescarse: %+v", info2)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if got := stream.written.String(); got != "S1\rS5\rS5\r" {
		t.Fatalf("GetInfo debía enviar S1 una vez y S5 siempre; envió %q", got)
	}
}

func TestSendResponsePreservesBytes(t *testing.T) {
	payload := []byte{0x00, 0x02, 0xFF, 0x0D}
	p, _ := newTestPrinter(t, string(payload))
	if err := p.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = p.Close() }()
	if err := p.SendCommand(context.Background(), []byte("S1\r")); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	resp, err := p.ReadResponse(context.Background())
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if !bytes.Equal(resp, payload) {
		t.Fatalf("respuesta alterada: %v != %v", resp, payload)
	}
}
