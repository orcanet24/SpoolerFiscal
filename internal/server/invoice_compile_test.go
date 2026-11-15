package server

import (
	"math"
	"strings"
	"testing"
)

func TestCompileInvoice(t *testing.T) {
	lines, totals, err := compileInvoice([]invoiceItem{
		{Description: "CAFE", Quantity: 1, UnitPrice: 10.00, Tax: "general"},
		{Description: "PAN", Quantity: 2, UnitPrice: 5.00, Tax: "reduced"},
	}, 500)
	if err != nil {
		t.Fatalf("compileInvoice: %v", err)
	}
	want := []string{
		"!   1.000     10.00CAFE",
		"\"   2.000      5.00PAN",
		"101",
	}
	if len(lines) != len(want) {
		t.Fatalf("líneas = %d (%q)", len(lines), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("línea %d = %q, se esperaba %q", i, lines[i], want[i])
		}
	}
	if math.Abs(totals.Total-20.00) > 0.001 {
		t.Fatalf("total = %.2f", totals.Total)
	}
	if math.Abs(totals.Tax-2.12) > 0.001 {
		t.Fatalf("iva = %.2f", totals.Tax)
	}
	if math.Abs(totals.Subtotal-17.88) > 0.001 {
		t.Fatalf("subtotal = %.2f", totals.Subtotal)
	}
}

func TestCompileInvoiceTaxClasses(t *testing.T) {
	lines, _, err := compileInvoice([]invoiceItem{
		{Description: "A", Quantity: 1, UnitPrice: 1, Tax: ""},
		{Description: "B", Quantity: 1, UnitPrice: 1, Tax: "additional"},
		{Description: "C", Quantity: 1, UnitPrice: 1, Tax: "exempt"},
		{Description: "D", Quantity: 1, UnitPrice: 1, Tax: "GENERAL"},
	}, 500)
	if err != nil {
		t.Fatalf("compileInvoice: %v", err)
	}
	prefixes := []string{"!", "#", " ", "!"}
	for i, p := range prefixes {
		if !strings.HasPrefix(lines[i], p) {
			t.Fatalf("línea %d = %q, prefijo esperado %q", i, lines[i], p)
		}
	}
}

func TestCompileInvoiceValidatesInput(t *testing.T) {
	cases := []struct {
		name  string
		items []invoiceItem
		want  string
	}{
		{"sin ítems", nil, "items es obligatorio"},
		{"descripción vacía", []invoiceItem{{Description: " ", Quantity: 1, UnitPrice: 1}}, "description es obligatoria"},
		{"inyección CRLF", []invoiceItem{{Description: "CAFE\r\n101", Quantity: 1, UnitPrice: 1}}, "ASCII imprimible"},
		{"separador", []invoiceItem{{Description: "A|B", Quantity: 1, UnitPrice: 1}}, "no puede contener"},
		{"descripción larga", []invoiceItem{{Description: strings.Repeat("X", 41), Quantity: 1, UnitPrice: 1}}, "excede 40"},
		{"cantidad cero", []invoiceItem{{Description: "A", Quantity: 0, UnitPrice: 1}}, "quantity debe estar"},
		{"cantidad enorme", []invoiceItem{{Description: "A", Quantity: 10000, UnitPrice: 1}}, "quantity debe estar"},
		{"precio negativo", []invoiceItem{{Description: "A", Quantity: 1, UnitPrice: -1}}, "unit_price debe estar"},
		{"alícuota inválida", []invoiceItem{{Description: "A", Quantity: 1, UnitPrice: 1, Tax: "iva21"}}, "tax"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := compileInvoice(tc.items, 500)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, se esperaba contener %q", err, tc.want)
			}
		})
	}
}

func TestCheckTotals(t *testing.T) {
	got := invoiceTotals{Subtotal: 17.88, Tax: 2.12, Total: 20.00}
	if err := checkTotals(invoiceTotals{Total: 20.00}, got); err != nil {
		t.Fatalf("total correcto rechazado: %v", err)
	}
	if err := checkTotals(invoiceTotals{Total: 20.005}, got); err != nil {
		t.Fatalf("tolerancia de centavos rechazada: %v", err)
	}
	if err := checkTotals(invoiceTotals{Total: 99}, got); err == nil {
		t.Fatal("un total distinto debía rechazarse")
	}
	if err := checkTotals(invoiceTotals{Total: 20, Subtotal: 10}, got); err == nil {
		t.Fatal("un subtotal distinto debía rechazarse")
	}
}
