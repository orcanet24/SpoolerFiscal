package fiscal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"go.bug.st/serial"

	"spoolerfiscal/internal/frame"
)

// SerialConfig son los parámetros del puerto serie físico de la impresora.
type SerialConfig struct {
	Name        string
	BaudRate    int
	DataBits    int
	Parity      string
	StopBits    string
	FlowControl string
}

type serialStream struct {
	port serial.Port
}

func (s *serialStream) Read(p []byte) (int, error)  { return s.port.Read(p) }
func (s *serialStream) Write(p []byte) (int, error) { return s.port.Write(p) }
func (s *serialStream) Close() error                { return s.port.Close() }
func (s *serialStream) SetReadTimeout(d time.Duration) error {
	return s.port.SetReadTimeout(d)
}

// OpenSerial abre el puerto serie físico con go.bug.st/serial.
func OpenSerial(cfg SerialConfig) (frame.Stream, error) {
	if cfg.Name == "" {
		return nil, errors.New("puerto serie sin nombre")
	}
	mode := &serial.Mode{
		BaudRate: cfg.BaudRate,
		DataBits: cfg.DataBits,
		Parity:   parityOf(cfg.Parity),
		StopBits: stopBitsOf(cfg.StopBits),
	}
	port, err := serial.Open(cfg.Name, mode)
	if err != nil {
		return nil, fmt.Errorf("abrir %s: %w", cfg.Name, err)
	}
	if err := port.SetReadTimeout(50 * time.Millisecond); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("configurar timeout de %s: %w", cfg.Name, err)
	}
	var st frame.Stream = &serialStream{port: port}
	if cfg.FlowControl == "xonxoff" {
		st = newXONXOFF(st)
	}
	return st, nil
}

func parityOf(p string) serial.Parity {
	switch p {
	case "odd":
		return serial.OddParity
	case "even":
		return serial.EvenParity
	case "mark":
		return serial.MarkParity
	case "space":
		return serial.SpaceParity
	default:
		return serial.NoParity
	}
}

func stopBitsOf(s string) serial.StopBits {
	switch s {
	case "1.5":
		return serial.OnePointFiveStopBits
	case "2":
		return serial.TwoStopBits
	default:
		return serial.OneStopBit
	}
}

// tcpStream envuelve una conexión TCP con la interfaz de stream serie. Se usa
// para laboratorio (simulador de impresora, ser2net, Moxa) y para las pruebas
// de integración, nunca sustituye al puerto físico en producción.
type tcpStream struct {
	conn net.Conn
}

// OpenTCP conecta con una impresora fiscal expuesta por TCP.
func OpenTCP(address string, dialTimeout time.Duration) (frame.Stream, error) {
	if dialTimeout <= 0 {
		dialTimeout = 3 * time.Second
	}
	conn, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("conectar con impresora en %s: %w", address, err)
	}
	return &tcpStream{conn: conn}, nil
}

func (t *tcpStream) Read(p []byte) (int, error) {
	n, err := t.conn.Read(p)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return 0, nil
		}
	}
	return n, err
}

func (t *tcpStream) Write(p []byte) (int, error) { return t.conn.Write(p) }
func (t *tcpStream) Close() error                { return t.conn.Close() }
func (t *tcpStream) SetReadTimeout(d time.Duration) error {
	return t.conn.SetReadDeadline(time.Now().Add(d))
}

// xonxoff aplica control de flujo por software: descarta XON/XOFF del flujo
// entrante y detiene las escrituras mientras el equipo remoto pide pausa.
// go.bug.st/serial no expone el handshake del driver, por eso XON/XOFF se
// resuelve en la capa de aplicación.
type xonxoff struct {
	stream  frame.Stream
	mu      sync.Mutex
	cond    *sync.Cond
	paused  bool
	maxWait time.Duration
}

func newXONXOFF(s frame.Stream) frame.Stream {
	x := &xonxoff{stream: s, maxWait: 5 * time.Second}
	x.cond = sync.NewCond(&x.mu)
	return x
}

func (x *xonxoff) Read(p []byte) (int, error) {
	buf := make([]byte, len(p))
	n, err := x.stream.Read(buf)
	if n <= 0 {
		return n, err
	}
	kept := 0
	x.mu.Lock()
	for _, b := range buf[:n] {
		switch b {
		case 0x13: // XOFF
			x.paused = true
		case 0x11: // XON
			x.paused = false
			x.cond.Broadcast()
		default:
			buf[kept] = b
			kept++
		}
	}
	x.mu.Unlock()
	return kept, nil
}

func (x *xonxoff) Write(p []byte) (int, error) {
	x.waitResume()
	return x.stream.Write(p)
}

func (x *xonxoff) waitResume() {
	deadline := time.Now().Add(x.maxWait)
	x.mu.Lock()
	for x.paused && time.Now().Before(deadline) {
		timer := time.AfterFunc(50*time.Millisecond, x.cond.Broadcast)
		x.cond.Wait()
		timer.Stop()
	}
	x.mu.Unlock()
}

func (x *xonxoff) Close() error { return x.stream.Close() }
func (x *xonxoff) SetReadTimeout(d time.Duration) error {
	return x.stream.SetReadTimeout(d)
}

// ContextReader espera cancelación del contexto mientras otro goroutine lee.
// No se usa para el puerto serie (los timeouts del driver bastan), pero queda
// disponible para transportes sin timeout nativo.
func waitContext(ctx context.Context, d time.Duration) error {
	if ctx == nil {
		time.Sleep(d)
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
