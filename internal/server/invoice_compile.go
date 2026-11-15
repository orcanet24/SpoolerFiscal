package server

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	// Límites del formato TFHKA: cantidad 8.3 y precio 10.2.
	maxItemDescription = 40
	maxItemQuantity    = 9999.999
	maxItemUnitPrice   = 9999999.99
	// Tolerancia al comparar los totales que envía el cliente.
	totalsTolerance = 0.011
)

// invoiceItem es una línea de factura en JSON.
type invoiceItem struct {
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	// general (16 %), reduced (8 %), additional (30 %) o exempt (0 %).
	// Vacío = general.
	Tax string `json:"tax,omitempty"`
}

// invoiceTotals son los totales calculados de la factura. Los precios unitarios
// se interpretan con IVA incluido, como los maneja la TFHKA.
type invoiceTotals struct {
	Subtotal float64 `json:"subtotal"`
	Tax      float64 `json:"tax"`
	Total    float64 `json:"total"`
}

type structuredInvoiceRequest struct {
	Station   string         `json:"station"`
	ClientRef string         `json:"client_ref,omitempty"`
	Items     []invoiceItem  `json:"items"`
	Totals    *invoiceTotals `json:"totals,omitempty"`
	TimeoutMS int            `json:"timeout_ms,omitempty"`
}

type structuredInvoiceResponse struct {
	invoiceResponse
	CompiledLines []string      `json:"compiled_lines"`
	Totals        invoiceTotals `json:"totals"`
}

type taxClass struct {
	prefix string
	rate   float64
}

var taxClasses = map[string]taxClass{
	"general":    {"!", 0.16},
	"reduced":    {"\"", 0.08},
	"additional": {"#", 0.30},
	"exempt":     {" ", 0},
}

// compileInvoice convierte ítems JSON en líneas TFHKA: una por ítem más el
// cierre 101. Devuelve también los totales calculados.
func compileInvoice(items []invoiceItem, maxItems int) ([]string, invoiceTotals, error) {
	var totals invoiceTotals
	if len(items) == 0 {
		return nil, totals, errors.New("items es obligatorio (al menos un ítem)")
	}
	if maxItems > 0 && len(items) > maxItems {
		return nil, totals, fmt.Errorf("demasiados ítems (máx. %d)", maxItems)
	}

	lines := make([]string, 0, len(items)+1)
	for i, it := range items {
		desc, err := cleanDescription(it.Description)
		if err != nil {
			return nil, totals, fmt.Errorf("ítem %d: %w", i, err)
		}
		qty := round3(it.Quantity)
		if qty <= 0 || qty > maxItemQuantity {
			return nil, totals, fmt.Errorf("ítem %d: quantity debe estar entre 0.001 y %.3f", i, maxItemQuantity)
		}
		price := round2(it.UnitPrice)
		if price < 0 || price > maxItemUnitPrice {
			return nil, totals, fmt.Errorf("ítem %d: unit_price debe estar entre 0 y %.2f", i, maxItemUnitPrice)
		}

		name := strings.ToLower(strings.TrimSpace(it.Tax))
		if name == "" {
			name = "general"
		}
		class, ok := taxClasses[name]
		if !ok {
			return nil, totals, fmt.Errorf("ítem %d: tax %q inválido (general|reduced|additional|exempt)", i, it.Tax)
		}

		nums := fmt.Sprintf("%8.3f%10.2f", qty, price)
		if len(nums) != 18 {
			return nil, totals, fmt.Errorf("ítem %d: cantidad o precio exceden el ancho del protocolo", i)
		}
		lines = append(lines, class.prefix+nums+desc)

		lineTotal := round2(qty * price)
		lineTax := round2(lineTotal - lineTotal/(1+class.rate))
		totals.Total += lineTotal
		totals.Tax += lineTax
	}
	lines = append(lines, "101")

	totals.Total = round2(totals.Total)
	totals.Tax = round2(totals.Tax)
	totals.Subtotal = round2(totals.Total - totals.Tax)
	return lines, totals, nil
}

// checkTotals compara los totales declarados por el cliente con los calculados.
// El total siempre se compara; subtotal e IVA sólo si vienen informados.
func checkTotals(want, got invoiceTotals) error {
	if math.Abs(want.Total-got.Total) > totalsTolerance {
		return fmt.Errorf("totals.total %.2f no coincide con el calculado %.2f", want.Total, got.Total)
	}
	if want.Subtotal > 0 && math.Abs(want.Subtotal-got.Subtotal) > totalsTolerance {
		return fmt.Errorf("totals.subtotal %.2f no coincide con el calculado %.2f", want.Subtotal, got.Subtotal)
	}
	if want.Tax > 0 && math.Abs(want.Tax-got.Tax) > totalsTolerance {
		return fmt.Errorf("totals.tax %.2f no coincide con el calculado %.2f", want.Tax, got.Tax)
	}
	return nil
}

// cleanDescription valida la descripción: ASCII imprimible, sin el separador
// `|` y sin CR/LF (evita inyectar comandos en la línea fiscal).
func cleanDescription(s string) (string, error) {
	desc := strings.TrimSpace(s)
	if desc == "" {
		return "", errors.New("description es obligatoria")
	}
	if len(desc) > maxItemDescription {
		return "", fmt.Errorf("description excede %d caracteres", maxItemDescription)
	}
	for _, r := range desc {
		if r < 0x20 || r > 0x7e {
			return "", errors.New("description debe ser ASCII imprimible (sin acentos ni saltos de línea)")
		}
		if r == '|' {
			return "", errors.New("description no puede contener '|'")
		}
	}
	return desc, nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
