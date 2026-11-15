package escpos

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"spoolerfiscal/internal/simprint"
)

func testConfig() simprint.Config {
	return simprint.Config{SerialID: "SIMTFHKA01", RIF: "J-12345678-9", Store: "TIENDA DE PRUEBA"}
}

func TestTicketContainsDocumentAndCut(t *testing.T) {
	doc := simprint.Document{
		Type:   "invoice",
		Number: 42,
		Total:  232.00,
		Tax:    32.00,
		Base:   200.00,
		Items: []simprint.Item{
			{Qty: 2, Price: 116, Desc: "HARINA 1KG", Rate: 0.16},
		},
		At: simprintNow(),
	}
	raw := Ticket(doc, testConfig())

	// Secuencias ESC/POS obligatorias.
	for _, seq := range [][]byte{
		{'\x1b', '@'},          // init
		{'\x1b', 'a', 1},       // centrado
		{'\x1b', 'E', 1},       // negrita
		{'\x1b', '!', 0x30},    // doble alto para el total
		{'\x1d', 'V', 0x42, 0}, // corte
	} {
		if !bytes.Contains(raw, seq) {
			t.Fatalf("falta la secuencia %q en: %q", seq, raw)
		}
	}

	text := string(raw)
	for _, want := range []string{"TIENDA DE PRUEBA", "RIF: J-12345678-9", "FACTURA No 42", "HARINA 1KG", "TOTAL:", "232.00", "NO FISCAL"} {
		if !strings.Contains(text, want) {
			t.Fatalf("ticket sin %q:\n%s", want, printable(text))
		}
	}
}

func TestTicketAsciiOnly(t *testing.T) {
	doc := simprint.Document{
		Type:   "invoice",
		Number: 1,
		Total:  116.00,
		Tax:    16.00,
		Base:   100.00,
		Items:  []simprint.Item{{Qty: 1, Price: 116, Desc: "MERCANCÍA ESPECIAL áéíóú", Rate: 0.16}},
		At:     simprintNow(),
	}
	raw := string(Ticket(doc, testConfig()))
	for _, r := range raw {
		if r != '\n' && r != '\r' && (r < 0x20 || r > 0x7e) {
			// Las secuencias de control ESC/POS (\x1b, \x1d) son válidas y
			// quedan fuera de ese rango: solo descartamos contenido de texto
			// no ASCII imprimible entre ellas.
			if !isControlByte(raw, r) {
				t.Fatalf("byte no ASCII imprimible: %q (U+%04X)", r, r)
			}
		}
	}
	if !strings.Contains(raw, "MERCANCIA ESPECIAL aeiou") {
		t.Fatalf("transliteración falló:\n%s", printable(raw))
	}
}

// isControlByte descarta los bytes de comando legítimos de ESC/POS.
func isControlByte(s string, r rune) bool {
	switch r {
	case '\x1b', '\x1d', 0x01, 0x30, 0x00, 0x42, 0x61, 0x45, 0x21, 0x56:
		return true
	}
	return false
}

func TestZReportTicket(t *testing.T) {
	doc := simprint.Document{Type: "zreport", Number: 3, Total: 1000, Tax: 160, Base: 840, Title: "Reporte Z", At: simprintNow()}
	raw := string(Ticket(doc, testConfig()))
	if !strings.Contains(raw, "CIERRE Z") || !strings.Contains(raw, "VENTAS TOTALES:") {
		t.Fatalf("reporte Z sin datos:\n%s", printable(raw))
	}
}

func printable(s string) string {
	return strings.ReplaceAll(s, "\x1b", "ESC")
}

func simprintNow() time.Time { return time.Date(2026, 11, 15, 10, 30, 0, 0, time.Local) }
