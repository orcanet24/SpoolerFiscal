// Package testutil: emulador IxBatch (Hasar/Bixolon/Epson) a nivel de bytes,
// espejo del simulador de referencia. No forma parte de los binarios de
// producción.
package testutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"spoolerfiscal/internal/frame"
)

// IxBatchSim emula una impresora IxBatch: responde los comandos @... y lleva
// correlativos reales por tipo de documento. Con brand="epson" envuelve cada
// respuesta con "0xESTADO|0xERROR|", como la impresora real.
type IxBatchSim struct {
	mu        sync.Mutex
	brand     string
	open      bool
	docType   string
	total     float64
	delay     time.Duration
	invoices  int64
	credit    int64
	debit     int64
	nonFiscal int64
	commands  []string
}

// NewIxBatchSim crea el emulador para una marca IxBatch.
func NewIxBatchSim(brand string) *IxBatchSim {
	return &IxBatchSim{brand: strings.ToLower(brand)}
}

// Handle procesa un comando crudo y devuelve la respuesta sin terminador.
func (s *IxBatchSim) Handle(raw string) string {
	cmd := strings.TrimSpace(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)

	if !strings.HasPrefix(cmd, "@") {
		return s.envelope("@Response|Error|0x50|Comando desconocido")
	}
	name, params := parseIxCommand(cmd)
	switch name {
	case "@status", "@statusrequest":
		doc := "CERRADO"
		if s.open {
			doc = "ABIERTO"
		}
		return s.envelope(fmt.Sprintf("@Response|OK|0|Status: 0x%02X|Error: 0x00|Doc: %s|Ventas: %.2f",
			s.statusByte(), doc, s.total))

	case "@openfiscalreceipt":
		s.openDoc("invoice")
		return s.envelope("@Response|OK|0|Documento fiscal abierto")
	case "@opencreditnote":
		s.openDoc("credit_note")
		return s.envelope("@Response|OK|0|Nota de credito abierta")
	case "@opendebitnote":
		s.openDoc("debit_note")
		return s.envelope("@Response|OK|0|Nota de debito abierta")
	case "@opennonfiscaldoc":
		s.openDoc("non_fiscal")
		return s.envelope("@Response|OK|0|Documento no fiscal abierto")

	case "@printlineitem", "@printline":
		if len(params) < 3 {
			return s.envelope("@Response|Error|2|Invalid params")
		}
		qty, err1 := strconv.ParseFloat(params[1], 64)
		price, err2 := strconv.ParseFloat(params[2], 64)
		if err1 != nil || err2 != nil {
			return s.envelope("@Response|Error|2|Invalid params")
		}
		if !s.open {
			s.openDoc("invoice")
		}
		lineTotal := qty * price
		s.total += lineTotal
		return s.envelope(fmt.Sprintf("@Response|OK|0|%s|%s|%s|%.2f",
			params[0], params[1], params[2], lineTotal))

	case "@close", "@closefiscalreceipt":
		if !s.open {
			return s.envelope("@Response|OK|0|Cerrado")
		}
		label, num := s.closeDoc()
		return s.envelope(fmt.Sprintf("@Response|OK|0|%s %d cerrada|Total: %.2f|Impreso:%d",
			label, num, s.total, num))

	case "@cancel":
		s.open = false
		s.docType = ""
		s.total = 0
		return s.envelope("@Response|OK|0|Cancelado")

	case "@closecashreceipt", "@closenonfiscaldoc":
		s.open = false
		s.docType = ""
		s.total = 0
		return s.envelope("@Response|OK|0|DNF cerrado")

	case "@printxreport":
		return s.envelope(fmt.Sprintf("@Response|OK|0|Reporte X #1|Ventas: %.2f|Impreso:1", s.total))
	case "@printzreport":
		return s.envelope(fmt.Sprintf("@Response|OK|0|Reporte Z #1|Ventas: %.2f|Impreso:1", s.total))

	default:
		return s.envelope("@Response|Error|0x50|Comando desconocido")
	}
}

func (s *IxBatchSim) openDoc(docType string) {
	s.open = true
	s.docType = docType
	s.total = 0
}

// closeDoc incrementa el correlativo del tipo y devuelve la etiqueta y el
// número, igual que la impresora de referencia.
func (s *IxBatchSim) closeDoc() (string, int64) {
	s.open = false
	docType := s.docType
	s.docType = ""
	switch docType {
	case "credit_note":
		s.credit++
		return "Nota de Credito", s.credit
	case "debit_note":
		s.debit++
		return "Nota de Debito", s.debit
	case "non_fiscal":
		s.nonFiscal++
		return "Documento", s.nonFiscal
	default:
		s.invoices++
		return "Factura", s.invoices
	}
}

func (s *IxBatchSim) statusByte() byte {
	if s.open {
		return 0x05
	}
	return 0x04
}

// envelope aplica el envoltorio de Epson; el resto de marcas responde el
// cuerpo tal cual.
func (s *IxBatchSim) envelope(response string) string {
	if s.brand != "epson" {
		return response
	}
	return fmt.Sprintf("0x%02X|0x%02X|%s", s.statusByte(), 0x00, response)
}

// SetDelay simula la latencia mecánica de la impresora por comando.
func (s *IxBatchSim) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

func (s *IxBatchSim) currentDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delay
}

// InvoiceCount devuelve las facturas emitidas por el emulador.
func (s *IxBatchSim) InvoiceCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invoices
}

// Commands devuelve los comandos recibidos.
func (s *IxBatchSim) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

// Serve atiende un stream hasta que el contexto se cancele.
func (s *IxBatchSim) Serve(ctx context.Context, st frame.Stream, gap time.Duration) error {
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

// StartTCPIxSim escucha en `addr` y atiende cada conexión con el emulador.
func StartTCPIxSim(ctx context.Context, addr string, gap time.Duration, brand string) (net.Listener, *IxBatchSim, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	sim := NewIxBatchSim(brand)
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

// parseIxCommand separa "@Comando|a|b" en nombre y parámetros.
func parseIxCommand(cmd string) (string, []string) {
	parts := strings.Split(cmd, "|")
	name := strings.ToLower(strings.TrimSpace(parts[0]))
	if len(parts) == 1 {
		return name, nil
	}
	params := make([]string, 0, len(parts)-1)
	for _, p := range parts[1:] {
		params = append(params, strings.TrimSpace(p))
	}
	return name, params
}
