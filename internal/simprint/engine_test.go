package simprint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	e, err := NewEngine(Config{SerialID: "SIM01", RIF: "J-1", Store: "TEST"}, path)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e, path
}

// itemCmd arma la línea de ítem TFHKA: prefijo + qty %8.3f + precio %10.2f.
func itemCmd(prefix, desc string, qty, price float64) string {
	return prefix + fmt.Sprintf("%8.3f%10.2f", qty, price) + desc
}

func TestInvoiceTotalsAndCorrelative(t *testing.T) {
	e, path := newTestEngine(t)
	var doc Document
	e.OnDocument = func(d Document) { doc = d }

	// 2 x 116.00 (16% incluido): total 232.00, IVA 32.00, base 200.00.
	if r := e.Handle(itemCmd("!", "HARINA 1KG", 2, 116)); r != "0|Item registrado" {
		t.Fatalf("item: %q", r)
	}
	resp := e.Handle("101")
	if !strings.HasPrefix(resp, "0|Factura 1|Total: 232.00|Impreso:1") {
		t.Fatalf("101: %q", resp)
	}
	if doc.Type != "invoice" || doc.Number != 1 || doc.Total != 232 || doc.Tax != 32 || doc.Base != 200 {
		t.Fatalf("documento: %+v", doc)
	}
	if len(doc.Items) != 1 {
		t.Fatalf("ítems: %d", len(doc.Items))
	}

	// La siguiente factura lleva el correlativo 2.
	e.OnDocument = nil
	if r := e.Handle(itemCmd("!", "ARROZ", 1, 58)); !strings.HasPrefix(r, "0|Item registrado") {
		t.Fatalf("item 2: %q", r)
	}
	if r := e.Handle("101"); !strings.Contains(r, "Factura 2") {
		t.Fatalf("correlativo: %q", r)
	}

	// El estado persiste: un motor nuevo sobre el mismo archivo continúa en 3.
	e2, err := NewEngine(Config{}, path)
	if err != nil {
		t.Fatalf("recargar: %v", err)
	}
	if e2.StateSnapshot().Invoices != 2 {
		t.Fatalf("correlativo persistido: %+v", e2.StateSnapshot())
	}
}

func TestPaperOutPausesPrinting(t *testing.T) {
	e, _ := newTestEngine(t)
	printed := 0
	e.OnDocument = func(Document) { printed++ }

	e.SetPaperOut(true)
	if r := e.Handle(itemCmd("!", "X", 1, 100)); r != "ERROR|0x01|Impresora fuera de papel" {
		t.Fatalf("item con papel agotado: %q", r)
	}
	if r := e.Handle("101"); r != "ERROR|0x01|Impresora fuera de papel" {
		t.Fatalf("101 con papel agotado: %q", r)
	}

	// Repuesto el papel, el cliente reenvía el ítem (como hace el spooler al
	// reanudar la cola) y el documento cierra bien.
	e.SetPaperOut(false)
	if r := e.Handle(itemCmd("!", "X", 1, 100)); !strings.HasPrefix(r, "0|Item") {
		t.Fatalf("reanudación del ítem: %q", r)
	}
	if r := e.Handle("101"); !strings.Contains(r, "Factura 1|Total: 100.00") {
		t.Fatalf("reanudación: %q", r)
	}
	if printed != 1 {
		t.Fatalf("documentos impresos: %d", printed)
	}
}

func TestS2ReportsStatus(t *testing.T) {
	e, _ := newTestEngine(t)

	if r := e.Handle("S2"); r != "0x04|0x00|0|OK" {
		t.Fatalf("S2 listo: %q", r)
	}
	e.Handle(itemCmd("!", "X", 1, 100))
	if r := e.Handle("S2"); !strings.HasPrefix(r, "0x02|0x00|") {
		t.Fatalf("S2 con documento abierto: %q", r)
	}
	e.SetPaperOut(true)
	if r := e.Handle("S2"); !strings.Contains(r, "|0x01|") {
		t.Fatalf("S2 con papel agotado: %q", r)
	}
}

func TestS1Identification(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Handle(itemCmd("!", "X", 1, 100))
	e.Handle("101")

	s1 := e.Handle("S1")
	if !strings.Contains(s1, "SIM01") || !strings.Contains(s1, "J-1") {
		t.Fatalf("S1 sin identificación: %q", s1)
	}
	// El correlativo de S1 refleja la factura emitida.
	if !strings.Contains(s1, "|1|") {
		t.Fatalf("S1 sin correlativo: %q", s1)
	}
}

func TestNonFiscalAndReports(t *testing.T) {
	e, _ := newTestEngine(t)

	if r := e.Handle("80$"); !strings.Contains(r, "DNF abierto") {
		t.Fatalf("80$: %q", r)
	}
	if r := e.Handle("81"); !strings.Contains(r, "Doc No Fiscal 1") {
		t.Fatalf("81: %q", r)
	}
	if st := e.StateSnapshot(); st.NonFiscal != 1 || st.Invoices != 0 {
		t.Fatalf("estado tras no fiscal: %+v", st)
	}

	if r := e.Handle("U0Z"); !strings.Contains(r, "Reporte Z #1") {
		t.Fatalf("U0Z: %q", r)
	}
}

func TestCreditNoteBy101(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Handle(itemCmd("!", "VENTA", 1, 116))
	e.Handle("101") // factura 1: total 116

	if r := e.Handle("d1NCF 001"); !strings.Contains(r, "NC abierta") {
		t.Fatalf("apertura NC: %q", r)
	}
	e.Handle(itemCmd("!", "DEVOLUCION", 1, 116))
	resp := e.Handle("101")
	if !strings.Contains(resp, "Nota de Credito 1") {
		t.Fatalf("cierre NC: %q", resp)
	}
	if st := e.StateSnapshot(); st.Invoices != 1 || st.Credit != 1 {
		t.Fatalf("correlativos tras NC: %+v", st)
	}
}

func TestUnknownCommandIsOK(t *testing.T) {
	e, _ := newTestEngine(t)
	if r := e.Handle("U711100"); r != "0|OK" {
		t.Fatalf("comando no soportado: %q", r)
	}
}

func TestParseItemMirrorsCompile(t *testing.T) {
	cmd := fmt.Sprintf("!%8.3f%10.2fHARINA", 2.0, 116.0)
	it, ok := parseItem(cmd)
	if !ok {
		t.Fatalf("parseItem rechazó: %q", cmd)
	}
	if it.Qty != 2 || it.Price != 116 || it.Desc != "HARINA" || it.Rate != 0.16 {
		t.Fatalf("ítem: %+v", it)
	}
	// Exento con prefijo espacio.
	cmd2 := fmt.Sprintf(" %8.3f%10.2fLECHE EXENTA", 1.0, 50.0)
	it2, ok := parseItem(cmd2)
	if !ok || it2.Rate != 0 {
		t.Fatalf("ítem exento: %+v ok=%v", it2, ok)
	}
	// Una línea corta no es ítem.
	if _, ok := parseItem("101"); ok {
		t.Fatal("101 no debe parsearse como ítem")
	}
}

func TestRenderTextFitsWidth(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Handle(itemCmd("!", strings.Repeat("PAN FRANCES DE LA CASA BLANCA INTEGRAL", 2), 2, 116))
	var doc Document
	e.OnDocument = func(d Document) { doc = d }
	e.Handle("101")

	text := RenderText(doc, Config{SerialID: "S1", RIF: "J-1", Store: "TIENDA"})
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if len(l) > Width {
			t.Fatalf("línea de %d columnas > %d: %q", len(l), Width, l)
		}
	}
	if !strings.Contains(text, "FACTURA No 1") {
		t.Fatalf("texto sin encabezado:\n%s", text)
	}
	if !strings.Contains(text, "TOTAL:") || !strings.Contains(text, "232.00") {
		t.Fatalf("texto sin total:\n%s", text)
	}
}

func TestSaveCreatesFile(t *testing.T) {
	e, path := newTestEngine(t)
	e.Handle(itemCmd("!", "X", 1, 100))
	e.Handle("101")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("estado no persistido: %v", err)
	}
}
