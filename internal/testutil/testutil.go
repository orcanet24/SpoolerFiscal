// Package testutil ofrece streams en memoria y un emulador TFHKA a nivel de
// bytes, usado por las pruebas unitarias y de integración. No forma parte de
// los binarios de producción.
package testutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"spoolerfiscal/internal/frame"
)

// ConnStream adapta un net.Conn a frame.Stream.
func ConnStream(c net.Conn) frame.Stream { return &connStream{Conn: c} }

type connStream struct{ net.Conn }

func (c *connStream) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return 0, nil
		}
	}
	return n, err
}

func (c *connStream) SetReadTimeout(d time.Duration) error {
	return c.Conn.SetReadDeadline(time.Now().Add(d))
}

// Pipe devuelve los dos extremos de un stream en memoria.
func Pipe() (frame.Stream, frame.Stream) {
	a, b := net.Pipe()
	return ConnStream(a), ConnStream(b)
}

// TFHKASim emula una impresora TFHKA a nivel de protocolo ASCII: responde los
// comandos y lleva correlativos reales.
type TFHKASim struct {
	mu        sync.Mutex
	open      bool
	delay     time.Duration
	invoices  int64
	credit    int64
	debit     int64
	nonFiscal int64
	commands  []string
}

// NewTFHKASim crea el emulador en cero.
func NewTFHKASim() *TFHKASim { return &TFHKASim{} }

// Handle procesa un comando crudo y devuelve la respuesta (sin terminador).
func (s *TFHKASim) Handle(raw string) string {
	cmd := strings.TrimSpace(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)

	switch {
	// Una factura se abre con la primera línea de ítem (prefijo de alícuota).
	case strings.HasPrefix(cmd, "!"), strings.HasPrefix(cmd, "\""), strings.HasPrefix(cmd, "#"):
		s.open = true
		return "0|Item registrado"
	case cmd == "101":
		if !s.open {
			return "0|Cerrado"
		}
		s.open = false
		s.invoices++
		return fmt.Sprintf("0|Factura %d|Total: 116.00|Impreso:%d", s.invoices, s.invoices)
	case cmd == "S1":
		return fmt.Sprintf("0|SIMTFHKA01|J-12345678-9|7|1|%d|%d|%d|%d|1160.00|2026-11-14T10:00:00",
			s.invoices, s.credit, s.debit, s.nonFiscal)
	case cmd == "S2":
		return "0|0|1|0.00|0.00|0.00|0"
	case cmd == "S5":
		return "SIMTFHKA01|J-12345678-9|1|4.00|3.60|7"
	case cmd == "U0X", cmd == "U1X":
		return "0|Reporte X #1|Ventas: 116.00|IVA: 16.00|Impreso:1"
	case cmd == "U0Z", cmd == "U1Z":
		return "0|Reporte Z #1|Ventas: 116.00|IVA: 16.00|Impreso:1"
	case cmd == "80$":
		return "0|DNF abierto"
	case cmd == "81":
		s.nonFiscal++
		return fmt.Sprintf("0|Doc No Fiscal %d|Total: 0.00", s.nonFiscal)
	case strings.HasPrefix(cmd, "d1"):
		s.credit++
		return fmt.Sprintf("0|Nota de Credito %d|Total: 0.00", s.credit)
	case strings.HasPrefix(cmd, "d2"):
		s.debit++
		return fmt.Sprintf("0|Nota de Debito %d|Total: 0.00", s.debit)
	default:
		return "0|OK"
	}
}

// SetDelay simula la latencia mecánica de la impresora por comando.
func (s *TFHKASim) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

func (s *TFHKASim) currentDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delay
}

// InvoiceCount devuelve las facturas emitidas por el emulador.
func (s *TFHKASim) InvoiceCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invoices
}

// Commands devuelve los comandos recibidos.
func (s *TFHKASim) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

// Serve atiende un stream hasta que el contexto se cancele.
func (s *TFHKASim) Serve(ctx context.Context, st frame.Stream, gap time.Duration) error {
	for {
		cmd, err := frame.Read(ctx, st, gap, 30*time.Second, 8192)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return nil
			case errors.Is(err, frame.ErrTimeout):
				continue
			default:
				return err
			}
		}
		if len(cmd) == 0 {
			continue
		}
		resp := s.Handle(string(cmd))
		if d := s.currentDelay(); d > 0 {
			time.Sleep(d)
		}
		if err := frame.WriteSettled(st, []byte(resp+"\r\n"), 0); err != nil {
			return err
		}
	}
}

// StartTCPSim escucha en `addr` y atiende cada conexión con el emulador.
func StartTCPSim(ctx context.Context, addr string, gap time.Duration) (net.Listener, *TFHKASim, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	sim := NewTFHKASim()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = sim.Serve(ctx, ConnStream(c), gap)
			}(conn)
		}
	}()
	return ln, sim, nil
}
