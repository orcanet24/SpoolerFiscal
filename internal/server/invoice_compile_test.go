package server

import (
	"math"
	"strings"
	"testing"
)

func TestCompileInvoice(t *testing.T) {
	lines, totals, err := compileInvoice(structuredInvoiceRequest{
		Items: []invoiceItem{
			{Description: "CAFE", Quantity: 1, UnitPrice: 10.00, Tax: "general"},
			{Description: "PAN", Quantity: 2, UnitPrice: 5.00, Tax: "reduced"},
		},
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
	lines, _, err := compileInvoice(structuredInvoiceRequest{
		Items: []invoiceItem{
			{Description: "A", Quantity: 1, UnitPrice: 1, Tax: ""},
			{Description: "B", Quantity: 1, UnitPrice: 1, Tax: "additional"},
			{Description: "C", Quantity: 1, UnitPrice: 1, Tax: "exempt"},
			{Description: "D", Quantity: 1, UnitPrice: 1, Tax: "GENERAL"},
		},
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

// Venta completa en el orden canónico de TFHKA: cliente, ítems, subtotal,
// descuento, recargo, medio de pago, pago y cierre.
func TestCompileInvoiceFullSale(t *testing.T) {
	lines, totals, err := compileInvoice(structuredInvoiceRequest{
		Customer: &invoiceCustomer{
			RIF:     "J-12345678-9",
			Name:    "EMPRESA DE PRUEBA C.A.",
			Address: "Av. Principal, Caracas",
		},
		Items: []invoiceItem{
			{Description: "HARINA 1KG MAIZ", Quantity: 2, UnitPrice: 50.00, Tax: "general"},
			{Description: "PAN TAJADO", Quantity: 3, UnitPrice: 10.50, Tax: "reduced"},
		},
		Discount:  10.00,
		Surcharge: 5.00,
		Payments: []invoicePayment{
			{Method: "transferencia", Amount: 126.50},
		},
	}, 500)
	if err != nil {
		t.Fatalf("compileInvoice: %v", err)
	}
	want := []string{
		"iR*J-12345678-9",
		"iS*EMPRESA DE PRUEBA C.A.",
		"i01Av. Principal, Caracas",
		"!   2.000     50.00HARINA 1KG MAIZ",
		"\"   3.000     10.50PAN TAJADO",
		"3",
		"70010.00",
		"7015.00",
		"103transferencia",
		"100126.50",
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
	if math.Abs(totals.Total-126.50) > 0.001 {
		t.Fatalf("total neto = %.2f", totals.Total)
	}
	if math.Abs(totals.Subtotal-115.38) > 0.001 || math.Abs(totals.Tax-16.12) > 0.001 {
		t.Fatalf("bruto = %+v", totals)
	}
	if totals.Discount != 10.00 || totals.Surcharge != 5.00 {
		t.Fatalf("ajustes = %+v", totals)
	}
}

// Pago en efectivo: no se declara 103 (la impresora asume efectivo).
func TestCompileInvoiceCashPaymentOmits103(t *testing.T) {
	lines, _, err := compileInvoice(structuredInvoiceRequest{
		Items:    []invoiceItem{{Description: "A", Quantity: 1, UnitPrice: 10, Tax: "general"}},
		Payments: []invoicePayment{{Amount: 10}},
	}, 500)
	if err != nil {
		t.Fatalf("compileInvoice: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "103") {
		t.Fatalf("no debía declarar medio de pago: %q", lines)
	}
	if !strings.Contains(joined, "3\n10010.00") {
		t.Fatalf("subtotal/pago ausentes: %q", lines)
	}
}

func TestCompileInvoiceValidatesInput(t *testing.T) {
	one := []invoiceItem{{Description: "A", Quantity: 1, UnitPrice: 10, Tax: "general"}}
	cases := []struct {
		name string
		req  structuredInvoiceRequest
		want string
	}{
		{"sin ítems", structuredInvoiceRequest{}, "items es obligatorio"},
		{"descripción vacía", structuredInvoiceRequest{Items: []invoiceItem{{Description: " ", Quantity: 1, UnitPrice: 1}}}, "description es obligatoria"},
		{"inyección CRLF", structuredInvoiceRequest{Items: []invoiceItem{{Description: "CAFE\r\n101", Quantity: 1, UnitPrice: 1}}}, "ASCII imprimible"},
		{"separador", structuredInvoiceRequest{Items: []invoiceItem{{Description: "A|B", Quantity: 1, UnitPrice: 1}}}, "no puede contener"},
		{"descripción larga", structuredInvoiceRequest{Items: []invoiceItem{{Description: strings.Repeat("X", 41), Quantity: 1, UnitPrice: 1}}}, "excede 40"},
		{"cantidad cero", structuredInvoiceRequest{Items: []invoiceItem{{Description: "A", Quantity: 0, UnitPrice: 1}}}, "quantity debe estar"},
		{"cantidad enorme", structuredInvoiceRequest{Items: []invoiceItem{{Description: "A", Quantity: 10000, UnitPrice: 1}}}, "quantity debe estar"},
		{"precio negativo", structuredInvoiceRequest{Items: []invoiceItem{{Description: "A", Quantity: 1, UnitPrice: -1}}}, "unit_price debe estar"},
		{"alícuota inválida", structuredInvoiceRequest{Items: []invoiceItem{{Description: "A", Quantity: 1, UnitPrice: 1, Tax: "iva21"}}}, "tax"},
		{"descuento negativo", structuredInvoiceRequest{Items: one, Discount: -1}, "discount debe estar"},
		{"recargo negativo", structuredInvoiceRequest{Items: one, Surcharge: -1}, "surcharge debe estar"},
		{"descuento mayor al total", structuredInvoiceRequest{Items: one, Discount: 20}, "excede el total"},
		{"pago corto", structuredInvoiceRequest{Items: one, Payments: []invoicePayment{{Amount: 5}}}, "no cubren el total"},
		{"pago cero", structuredInvoiceRequest{Items: one, Payments: []invoicePayment{{Amount: 0}}}, "amount debe estar"},
		{"medio con separador", structuredInvoiceRequest{Items: one, Payments: []invoicePayment{{Method: "tarjeta|101", Amount: 10}}}, "no puede contener"},
		{"cliente con CRLF", structuredInvoiceRequest{Items: one, Customer: &invoiceCustomer{Name: "A\r\n101"}}, "ASCII imprimible"},
		{"cliente largo", structuredInvoiceRequest{Items: one, Customer: &invoiceCustomer{Name: strings.Repeat("X", 41)}}, "excede 40"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := compileInvoice(tc.req, 500)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, se esperaba contener %q", err, tc.want)
			}
		})
	}
}

func TestCompileInvoiceRespectsMaxLines(t *testing.T) {
	req := structuredInvoiceRequest{
		Customer: &invoiceCustomer{RIF: "J-1", Name: "N", Address: "A", Phone: "P"},
		Items:    []invoiceItem{{Description: "A", Quantity: 1, UnitPrice: 10}},
	}
	// 4 de cliente + 1 ítem + 101 = 6 líneas.
	if _, _, err := compileInvoice(req, 5); err == nil {
		t.Fatal("debía rechazar por límite de líneas")
	}
	if _, _, err := compileInvoice(req, 6); err != nil {
		t.Fatalf("6 líneas debía aceptar: %v", err)
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

	// Con descuento el desglose puede variar: sólo manda el total.
	withDiscount := invoiceTotals{Subtotal: 17.88, Tax: 2.12, Discount: 2, Total: 18.00}
	if err := checkTotals(invoiceTotals{Total: 18.00, Subtotal: 999, Tax: 999}, withDiscount); err != nil {
		t.Fatalf("con descuento no debía comparar el desglose: %v", err)
	}
}
