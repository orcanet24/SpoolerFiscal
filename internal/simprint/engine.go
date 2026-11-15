// Package simprint emula una impresora fiscal TFHKA a nivel de protocolo ASCII
// con salida a papel real: escucha los comandos del spooler (o de Profit Plus)
// y, al cerrar cada documento, entrega el ticket para imprimirlo en una
// impresora térmica común vía ESC/POS.
//
// El resultado NO es un documento fiscal válido: es un simulador para probar
// el circuito completo (correlativo, colas, pausa por papel, reintentos) con
// papel de verdad.
package simprint

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State son los correlativos del emulador, equivalentes a la memoria fiscal.
type State struct {
	Invoices  int64   `json:"invoices"`
	Credit    int64   `json:"credit_notes"`
	Debit     int64   `json:"debit_notes"`
	NonFiscal int64   `json:"non_fiscal"`
	Reports   int64   `json:"reports"`
	Total     float64 `json:"total_billed"`
	Tax       float64 `json:"tax_billed"`
}

// Item es una línea de ítem acumulada en el documento abierto.
type Item struct {
	Qty   float64
	Price float64 // con IVA incluido, como la TFHKA
	Desc  string
	Rate  float64 // alícuota: 0.16, 0.08, 0.30 o 0
}

// Document es un documento que acaba de cerrarse; lo recibe la salida de
// impresión (ESC/POS, texto o archivo).
type Document struct {
	Type   string // invoice | credit | debit | nonfiscal | report | zreport
	Number int64
	Total  float64
	Tax    float64
	Base   float64
	Items  []Item
	Title  string // texto del reporte (X/Z)
	At     time.Time
}

// Config identifica al emulador.
type Config struct {
	SerialID string
	RIF      string
	Store    string
}

// Engine implementa el protocolo TFHKA (comandos → respuestas) y lleva el
// correlativo. Concurrency-safe.
type Engine struct {
	cfg Config

	mu       sync.Mutex
	state    State
	items    []Item
	docOpen  bool
	docType  string
	paperOut bool

	statePath  string
	OnDocument func(Document)
}

// NewEngine crea el emulador y carga el estado si el archivo existe.
func NewEngine(cfg Config, statePath string) (*Engine, error) {
	e := &Engine{cfg: cfg, statePath: statePath}
	if statePath != "" {
		if data, err := os.ReadFile(statePath); err == nil {
			if err := json.Unmarshal(data, &e.state); err != nil {
				return nil, fmt.Errorf("estado del emulador %s: %w", statePath, err)
			}
		}
	}
	return e, nil
}

// Save persiste los correlativos (temporal + renombrar, atómico).
func (e *Engine) Save() error {
	if e.statePath == "" {
		return nil
	}
	e.mu.Lock()
	data, err := json.MarshalIndent(e.state, "", "  ")
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.statePath), 0o755); err != nil {
		return err
	}
	tmp := e.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, e.statePath)
}

// SetPaperOut simula el sensor de papel (para probar la pausa y reanudación
// de la cola). Devuelve el estado nuevo.
func (e *Engine) SetPaperOut(on bool) bool {
	e.mu.Lock()
	e.paperOut = on
	st := on
	e.mu.Unlock()
	return st
}

// PaperOut consulta el sensor simulado.
func (e *Engine) PaperOut() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paperOut
}

// DocOpen informa si hay un documento fiscal abierto (conviene consultarlo
// para el S2).
func (e *Engine) DocOpen() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.docOpen
}

// StateSnapshot devuelve copia de los correlativos.
func (e *Engine) StateSnapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

var errPaperOut = errors.New("papel agotado")

// Handle procesa un comando crudo y devuelve la respuesta TFHKA (sin
// terminador), igual que la impresora real.
func (e *Engine) Handle(raw string) string {
	cmd := strings.TrimSpace(raw)

	if item, ok := parseItem(cmd); ok {
		return e.handleItem(item)
	}
	switch cmd {
	case "101":
		return e.closeDoc("invoice", "Factura")
	case "S1":
		return e.handleS1()
	case "S2":
		return e.handleS2()
	case "S5":
		return e.handleS5()
	case "U0X", "U1X":
		return e.report("report", "Reporte X")
	case "U0Z", "U1Z":
		return e.report("zreport", "Reporte Z")
	case "80$":
		return e.openNonFiscal()
	case "81":
		return e.closeDoc("nonfiscal", "Doc No Fiscal")
	case "I0":
		return "0|OK"
	}
	if strings.HasPrefix(cmd, "d1") {
		return e.handleCredit(cmd)
	}
	if strings.HasPrefix(cmd, "d2") {
		return e.handleDebit(cmd)
	}
	return "0|OK"
}

// ---------- ítems ----------

// parseItem interpreta la línea de ítem TFHKA: prefijo de alícuota (1 carácter)
// + cantidad %8.3f + precio %10.2f + descripción. Espeja compileInvoice del
// servidor, que usa exactamente esos anchos.
func parseItem(cmd string) (Item, bool) {
	if len(cmd) < 19 {
		return Item{}, false
	}
	var rate float64
	switch cmd[0] {
	case '!':
		rate = 0.16
	case '"':
		rate = 0.08
	case '#':
		rate = 0.30
	case ' ':
		rate = 0
	default:
		return Item{}, false
	}
	var qty, price float64
	if _, err := fmt.Sscanf(cmd[1:9], "%8f", &qty); err != nil || qty <= 0 {
		return Item{}, false
	}
	if _, err := fmt.Sscanf(cmd[9:19], "%10f", &price); err != nil || price < 0 {
		return Item{}, false
	}
	desc := strings.TrimSpace(cmd[19:])
	if desc == "" {
		desc = "ITEM"
	}
	return Item{Qty: qty, Price: price, Desc: desc, Rate: rate}, true
}

func (e *Engine) handleItem(item Item) string {
	if e.PaperOut() {
		return "ERROR|0x01|Impresora fuera de papel"
	}
	e.mu.Lock()
	if !e.docOpen {
		e.docOpen = true
		e.docType = "invoice"
		e.items = nil
	}
	e.items = append(e.items, item)
	e.mu.Unlock()
	return "0|Item registrado"
}

func (e *Engine) totalsOf(items []Item) (total, tax, base float64) {
	for _, it := range items {
		line := round2(it.Qty * it.Price)
		iva := round2(line - line/(1+it.Rate))
		total += line
		tax += iva
	}
	return round2(total), round2(tax), round2(total - tax)
}

// ---------- documentos ----------

// closeDoc cierra el documento abierto (factura, NC, ND o no fiscal), lo
// entrega a la salida de impresión y responde como la TFHKA. El tipo real
// depende de lo que se abrió: la NC/ND (d1/d2) también se cierra con 101.
func (e *Engine) closeDoc(docType, label string) string {
	if e.PaperOut() {
		return "ERROR|0x01|Impresora fuera de papel"
	}
	e.mu.Lock()
	docOpen, accType := e.docOpen, e.docType
	items := append([]Item(nil), e.items...)
	e.docOpen = false
	e.items = nil
	e.mu.Unlock()

	if !docOpen {
		return "0|Cerrado"
	}
	if docType == "invoice" {
		switch accType {
		case "nonfiscal", "credit", "debit":
			docType = accType
		}
	}
	switch docType {
	case "credit":
		label = "Nota de Credito"
	case "debit":
		label = "Nota de Debito"
	}
	total, tax, base := e.totalsOf(items)

	e.mu.Lock()
	var number int64
	switch docType {
	case "invoice":
		e.state.Invoices++
		number = e.state.Invoices
		e.state.Total = round2(e.state.Total + total)
		e.state.Tax = round2(e.state.Tax + tax)
	case "credit":
		e.state.Credit++
		number = e.state.Credit
		e.state.Total = round2(e.state.Total - total)
		e.state.Tax = round2(e.state.Tax - tax)
	case "debit":
		e.state.Debit++
		number = e.state.Debit
		e.state.Total = round2(e.state.Total + total)
		e.state.Tax = round2(e.state.Tax + tax)
	default:
		e.state.NonFiscal++
		number = e.state.NonFiscal
	}
	e.mu.Unlock()
	e.persist()

	doc := Document{Type: docType, Number: number, Total: total, Tax: tax, Base: base, Items: items, At: time.Now()}
	if e.OnDocument != nil {
		e.OnDocument(doc)
	}
	if docType == "invoice" {
		return fmt.Sprintf("0|Factura %d|Total: %.2f|Impreso:%d", number, total, number)
	}
	return fmt.Sprintf("0|%s %d|Total: %.2f|Impreso:%d", label, number, total, number)
}

func (e *Engine) openNonFiscal() string {
	if e.PaperOut() {
		return "ERROR|0x01|Impresora fuera de papel"
	}
	e.mu.Lock()
	e.docOpen = true
	e.docType = "nonfiscal"
	e.items = nil
	e.mu.Unlock()
	return "0|DNF abierto"
}

func (e *Engine) handleCredit(cmd string) string {
	// d1... es la Nota de Crédito: se abre como documento propio.
	if e.PaperOut() {
		return "ERROR|0x01|Impresora fuera de papel"
	}
	e.mu.Lock()
	e.docOpen = true
	e.docType = "credit"
	e.items = nil
	e.mu.Unlock()
	return "0|NC abierta"
}

func (e *Engine) handleDebit(cmd string) string {
	if e.PaperOut() {
		return "ERROR|0x01|Impresora fuera de papel"
	}
	e.mu.Lock()
	e.docOpen = true
	e.docType = "debit"
	e.items = nil
	e.mu.Unlock()
	return "0|ND abierta"
}

func (e *Engine) report(kind, label string) string {
	if e.PaperOut() {
		return "ERROR|0x01|Impresora fuera de papel"
	}
	e.mu.Lock()
	e.state.Reports++
	number := e.state.Reports
	total, tax := e.state.Total, e.state.Tax
	base := round2(total - tax)
	e.mu.Unlock()

	doc := Document{Type: kind, Number: number, Total: total, Tax: tax, Base: base, Title: label, At: time.Now()}
	if e.OnDocument != nil {
		e.OnDocument(doc)
	}
	e.persist()
	return fmt.Sprintf("0|%s #%d|Ventas: %.2f|IVA: %.2f|Impreso:%d", label, number, total, tax, number)
}

// ---------- consultas ----------

func (e *Engine) handleS1() string {
	e.mu.Lock()
	st := e.state
	e.mu.Unlock()
	return fmt.Sprintf("0|%s|%s|7|1|%d|%d|%d|%d|1160.00|%s",
		e.idOr("SIMTFHKA01"), e.rifOr("J-12345678-9"),
		st.Invoices, st.Credit, st.Debit, st.NonFiscal,
		time.Now().Format("2006-01-02T15:04:05"))
}

// handleS2 responde con el par de bytes de estado que el driver TFHKA parsea:
// 0x04 = listo, 0x02 = documento abierto, 0x01 = papel agotado (error byte).
func (e *Engine) handleS2() string {
	e.mu.Lock()
	docOpen, paperOut := e.docOpen, e.paperOut
	e.mu.Unlock()
	sb := byte(0x04)
	if docOpen {
		sb = 0x02
	}
	eb := byte(0x00)
	if paperOut {
		eb = 0x01
	}
	return fmt.Sprintf("0x%02X|0x%02X|0|OK", sb, eb)
}

func (e *Engine) handleS5() string {
	return fmt.Sprintf("%s|%s|1|4.00|3.60|7", e.idOr("SIMTFHKA01"), e.rifOr("J-12345678-9"))
}

func (e *Engine) idOr(def string) string {
	if e.cfg.SerialID != "" {
		return e.cfg.SerialID
	}
	return def
}

func (e *Engine) rifOr(def string) string {
	if e.cfg.RIF != "" {
		return e.cfg.RIF
	}
	return def
}

func (e *Engine) persist() {
	if e.statePath == "" {
		return
	}
	if err := e.Save(); err != nil {
		// El caller no puede manejarlo de forma útil: el estado es best effort.
		_ = err
	}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
