package fiscal

import (
	"bytes"
	"strings"
	"time"

	"spoolerfiscal/internal/frame"
)

// El protocolo serie de The Factory HKA (TFHKA) no es texto plano: cada
// comando viaja en una trama
//
//	STX  <comando>  ETX  LRC
//
// donde LRC es el XOR de los bytes del comando más el ETX (el STX no entra en
// el cálculo). La impresora contesta con la misma envoltura:
//
//	STX  <datos>  ETX  LRC
//
// o con un único byte NAK (0x15) cuando rechaza el comando. Sin esta trama la
// impresora no contesta nada: es la razón por la que "S1\r\n" en texto plano
// se quedaba sin respuesta.
const (
	tfhkaSTX = 0x02
	tfhkaETX = 0x03
	tfhkaNAK = 0x15
)

// tfhkaFrame envuelve un comando en su trama STX/ETX/LRC.
func tfhkaFrame(cmd []byte) []byte {
	cmd = bytes.TrimRight(cmd, "\r\n")
	out := make([]byte, 0, len(cmd)+3)
	out = append(out, tfhkaSTX)
	out = append(out, cmd...)
	out = append(out, tfhkaETX)
	var lrc byte
	for _, b := range out[1:] { // comando + ETX, sin STX
		lrc ^= b
	}
	return append(out, lrc)
}

// stripTFHKAFrame quita STX, ETX y LRC de una respuesta. Si la respuesta es un
// NAK suelto se devuelve tal cual para que Describe lo reporte como error. Si
// la trama llegó incompleta (sin ETX) se devuelve lo recibido.
func stripTFHKAFrame(b []byte) []byte {
	if len(b) == 1 && b[0] == tfhkaNAK {
		return b
	}
	i := bytes.IndexByte(b, tfhkaSTX)
	if i < 0 {
		return b
	}
	rest := b[i+1:]
	if j := bytes.IndexByte(rest, tfhkaETX); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// tfhkaFramer adapta un flujo serie al framing TFHKA. Cada línea del payload
// se envía como una trama independiente (una factura completa puede llegar en
// varias líneas y la impresora exige un frame por comando). Las lecturas se
// devuelven sin tocar; Describe y los parsers descartan la envoltura.
type tfhkaFramer struct {
	stream frame.Stream
}

func newTFHKAFramer(s frame.Stream) frame.Stream { return &tfhkaFramer{stream: s} }

// isTFHKAControlOnly indica si el payload está hecho sólo de bytes de control
// que el protocolo HKA intercambia fuera de la trama STX/ETX/LRC: ENQ (0x05),
// ACK (0x06), NAK (0x15) y control de flujo por software XON (0x11) / XOFF
// (0x13). Estos bytes no se envuelven; ver tfhkaFramer.Write.
func isTFHKAControlOnly(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		switch c {
		case 0x05, 0x06, 0x15, 0x11, 0x13:
		default:
			return false
		}
	}
	return true
}

func (f *tfhkaFramer) Write(p []byte) (int, error) {
	// El terminador que agrega normalizeCommand (CR/LF) no forma parte de la
	// trama: se descarta aquí para no alterar ni el framing ni el handshake.
	trimmed := bytes.TrimRight(p, "\r\n")
	if len(trimmed) == 0 {
		return len(p), nil
	}
	// ENQ/ACK/NAK/XON/XOFF viajan sueltos. El SDK de The Factory (Profit Plus y
	// el demo) los intercambia fuera de STX/ETX/LRC; envolverlos hace que la
	// impresora conteste NAK (0x15) y el cliente lo reporte como "No Answer".
	if isTFHKAControlOnly(trimmed) {
		if err := frame.Write(f.stream, trimmed); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	// Si el cliente ya habla el protocolo (la trama llega con STX, como cuando
	// Profit Plus escribe en el COM virtual) se envía tal cual: el spooler no
	// debe re-envolver lo que ya está enmarcado.
	if bytes.IndexByte(trimmed, tfhkaSTX) >= 0 {
		if err := frame.Write(f.stream, trimmed); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	text := strings.ReplaceAll(string(trimmed), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	sent := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := frame.Write(f.stream, tfhkaFrame([]byte(line))); err != nil {
			return 0, err
		}
		sent = true
	}
	if !sent {
		if err := frame.Write(f.stream, tfhkaFrame(p)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (f *tfhkaFramer) Read(p []byte) (int, error) { return f.stream.Read(p) }
func (f *tfhkaFramer) Close() error               { return f.stream.Close() }
func (f *tfhkaFramer) SetReadTimeout(d time.Duration) error {
	return f.stream.SetReadTimeout(d)
}

// FrameSTX y UnframeSTX exponen el framing para emuladores y pruebas que
// necesitan hablar el mismo protocolo que la impresora real.
func FrameSTX(cmd []byte) []byte    { return tfhkaFrame(cmd) }
func UnframeSTX(resp []byte) []byte { return stripTFHKAFrame(resp) }
