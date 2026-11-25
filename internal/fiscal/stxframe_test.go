package fiscal

import (
	"bytes"
	"testing"
	"time"
)

// captureStream es un frame.Stream mínimo que sólo registra lo escrito.
type captureStream struct{ written bytes.Buffer }

func (c *captureStream) Read(p []byte) (int, error)           { return 0, nil }
func (c *captureStream) Write(p []byte) (int, error)          { return c.written.Write(p) }
func (c *captureStream) Close() error                         { return nil }
func (c *captureStream) SetReadTimeout(_ time.Duration) error { return nil }

// TestTFHKAFramerPassesControlBytesRaw garantiza que ENQ/ACK/NAK viajen sin la
// envoltura STX/ETX/LRC. El SDK de The Factory (Profit Plus y el demo Venezuela)
// intercambia estos bytes sueltos para el handshake de estado y de reportes; si
// se envuelven, la impresora contesta NAK (0x15) y el cliente reporta
// "No Answer" (0x89).
func TestTFHKAFramerPassesControlBytesRaw(t *testing.T) {
	cases := [][]byte{
		{0x05},
		{0x06},
		{0x15},
		{0x11, 0x13},
		{0x05, '\r', '\n'}, // normalizeCommand les añade el terminador
	}
	for _, in := range cases {
		stream := &captureStream{}
		if _, err := newTFHKAFramer(stream).Write(in); err != nil {
			t.Fatalf("Write(%x): %v", in, err)
		}
		want := bytes.TrimRight(in, "\r\n")
		if got := stream.written.Bytes(); !bytes.Equal(got, want) {
			t.Errorf("Write(%x) escribió %x, se esperaba %x (sin envoltura)", in, got, want)
		}
	}
}

// TestTFHKAFramerWrapsTextCommands conserva el framing obligatorio para las
// líneas de texto (reportes, ítems, cierres).
func TestTFHKAFramerWrapsTextCommands(t *testing.T) {
	stream := &captureStream{}
	if _, err := newTFHKAFramer(stream).Write([]byte("S1")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := stream.written.Bytes()
	// STX 'S' '1' ETX LRC; LRC = 'S' ^ '1' ^ ETX = 0x61 (el STX no entra).
	if len(got) != 5 || got[0] != tfhkaSTX || got[1] != 'S' || got[2] != '1' || got[3] != tfhkaETX || got[4] != 0x61 {
		t.Fatalf("S1 = %x, se esperaba 02 53 31 03 61", got)
	}
}

// TestTFHKAFramerPassesPreframedUnchanged: si el cliente ya manda la trama HKA
// (Profit Plus), el framer no la re-envuelve.
func TestTFHKAFramerPassesPreframedUnchanged(t *testing.T) {
	frameBytes := FrameSTX([]byte("S1"))
	stream := &captureStream{}
	if _, err := newTFHKAFramer(stream).Write(append(append([]byte{}, frameBytes...), '\r', '\n')); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := stream.written.Bytes(); !bytes.Equal(got, frameBytes) {
		t.Fatalf("escrito %x, se esperaba %x", got, frameBytes)
	}
}
