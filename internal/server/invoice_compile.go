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
	// Datos del cliente: anchos de la línea fiscal (40 columnas típicas).
	maxCustomerRIF     = 20
	maxCustomerName    = 40
	maxCustomerAddress = 80
	maxCustomerPhone   = 20
	// Texto libre del medio de pago (103<medio>).
	maxPaymentMethod = 20
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

// invoiceCustomer son los datos del cliente. Todos los campos son opcionales;
// los vacíos no se envían a la impresora. Se emiten como iR*/iS*/i01/i02.
type invoiceCustomer struct {
	RIF     string `json:"rif,omitempty"`
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
	Phone   string `json:"phone,omitempty"`
}

// invoicePayment es una forma de pago. method vacío o "cash" no declara 103
// (la impresora asume efectivo); otro texto se envía como descripción libre.
type invoicePayment struct {
	Method string  `json:"method,omitempty"`
	Amount float64 `json:"amount"`
}

// invoiceTotals son los totales calculados de la factura. Los precios unitarios
// se interpretan con IVA incluido, como los maneja la TFHKA. Subtotal y Tax son
// los del bruto de los ítems; Discount y Surcharge son globales y Total ya los
// aplica.
type invoiceTotals struct {
	Subtotal  float64 `json:"subtotal"`
	Tax       float64 `json:"tax"`
	Discount  float64 `json:"discount,omitempty"`
	Surcharge float64 `json:"surcharge,omitempty"`
	Total     float64 `json:"total"`
}

type structuredInvoiceRequest struct {
	Station   string           `json:"station"`
	ClientRef string           `json:"client_ref,omitempty"`
	Customer  *invoiceCustomer `json:"customer,omitempty"`
	Items     []invoiceItem    `json:"items"`
	// Descuento y recargo globales (700/701), en monto.
	Discount  float64 `json:"discount,omitempty"`
	Surcharge float64 `json:"surcharge,omitempty"`
	// Formas de pago; si vienen, se imprime el subtotal (3) antes del pago.
	Payments  []invoicePayment `json:"payments,omitempty"`
	Totals    *invoiceTotals   `json:"totals,omitempty"`
	TimeoutMS int              `json:"timeout_ms,omitempty"`
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

// compileInvoice convierte la solicitud en líneas TFHKA en el orden canónico
// del protocolo: datos del cliente, ítems, subtotal, descuento/recargo, pagos y
// cierre. Devuelve también los totales calculados.
func compileInvoice(req structuredInvoiceRequest, maxLines int) ([]string, invoiceTotals, error) {
	var totals invoiceTotals
	if len(req.Items) == 0 {
		return nil, totals, errors.New("items es obligatorio (al menos un ítem)")
	}
	if maxLines > 0 && len(req.Items) > maxLines {
		return nil, totals, fmt.Errorf("demasiados ítems (máx. %d)", maxLines)
	}

	customerLines, err := compileCustomer(req.Customer)
	if err != nil {
		return nil, totals, err
	}
	itemLines, itemTotals, err := compileItems(req.Items)
	if err != nil {
		return nil, totals, err
	}
	totals = itemTotals

	discount := round2(req.Discount)
	surcharge := round2(req.Surcharge)
	if discount < 0 || discount > maxItemUnitPrice {
		return nil, totals, fmt.Errorf("discount debe estar entre 0 y %.2f", float64(maxItemUnitPrice))
	}
	if surcharge < 0 || surcharge > maxItemUnitPrice {
		return nil, totals, fmt.Errorf("surcharge debe estar entre 0 y %.2f", float64(maxItemUnitPrice))
	}
	net := round2(totals.Total - discount + surcharge)
	if net < 0 {
		return nil, totals, fmt.Errorf("el descuento (%.2f) excede el total de la factura (%.2f)", discount, totals.Total)
	}

	paymentLines, err := compilePayments(req.Payments, net)
	if err != nil {
		return nil, totals, err
	}

	lines := make([]string, 0, len(customerLines)+len(itemLines)+len(paymentLines)+4)
	lines = append(lines, customerLines...)
	lines = append(lines, itemLines...)
	if len(req.Payments) > 0 {
		// Subtotal impreso, como la venta completa de referencia.
		lines = append(lines, "3")
	}
	if discount > 0 {
		lines = append(lines, fmt.Sprintf("700%.2f", discount))
	}
	if surcharge > 0 {
		lines = append(lines, fmt.Sprintf("701%.2f", surcharge))
	}
	lines = append(lines, paymentLines...)
	lines = append(lines, "101")

	if maxLines > 0 && len(lines) > maxLines {
		return nil, totals, fmt.Errorf("la factura compilada excede %d líneas", maxLines)
	}

	totals.Discount = discount
	totals.Surcharge = surcharge
	totals.Total = net
	return lines, totals, nil
}

// compileCustomer valida y arma los comandos de datos del cliente. El orden
// (RIF, nombre, dirección, teléfono) es el canónico de TFHKA.
func compileCustomer(c *invoiceCustomer) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	rif, err := cleanField(c.RIF, maxCustomerRIF, "customer.rif")
	if err != nil {
		return nil, err
	}
	name, err := cleanField(c.Name, maxCustomerName, "customer.name")
	if err != nil {
		return nil, err
	}
	address, err := cleanField(c.Address, maxCustomerAddress, "customer.address")
	if err != nil {
		return nil, err
	}
	phone, err := cleanField(c.Phone, maxCustomerPhone, "customer.phone")
	if err != nil {
		return nil, err
	}
	var lines []string
	if rif != "" {
		lines = append(lines, "iR*"+rif)
	}
	if name != "" {
		lines = append(lines, "iS*"+name)
	}
	if address != "" {
		lines = append(lines, "i01"+address)
	}
	if phone != "" {
		lines = append(lines, "i02"+phone)
	}
	return lines, nil
}

// compileItems valida los ítems y calcula sus totales (brutos, con IVA
// incluido). No agrega el cierre: lo hace compileInvoice.
func compileItems(items []invoiceItem) ([]string, invoiceTotals, error) {
	var totals invoiceTotals
	lines := make([]string, 0, len(items))
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
	totals.Total = round2(totals.Total)
	totals.Tax = round2(totals.Tax)
	totals.Subtotal = round2(totals.Total - totals.Tax)
	return lines, totals, nil
}

// compilePayments valida las formas de pago y arma 103<medio>/100<monto>. Si el
// total pagado no cubre la factura se rechaza antes de imprimir; si sobra, es
// vuelto y la impresora lo calcula.
func compilePayments(payments []invoicePayment, net float64) ([]string, error) {
	if len(payments) == 0 {
		return nil, nil
	}
	var lines []string
	var paid float64
	for i, p := range payments {
		amt := round2(p.Amount)
		if amt <= 0 || amt > maxItemUnitPrice {
			return nil, fmt.Errorf("payments[%d]: amount debe estar entre 0.01 y %.2f", i, float64(maxItemUnitPrice))
		}
		method, err := cleanField(p.Method, maxPaymentMethod, fmt.Sprintf("payments[%d].method", i))
		if err != nil {
			return nil, err
		}
		if method != "" && !strings.EqualFold(method, "cash") {
			lines = append(lines, "103"+method)
		}
		lines = append(lines, fmt.Sprintf("100%.2f", amt))
		paid += amt
	}
	if round2(paid)+totalsTolerance < net {
		return nil, fmt.Errorf("los pagos (%.2f) no cubren el total de la factura (%.2f)", round2(paid), net)
	}
	return lines, nil
}

// checkTotals compara los totales declarados por el cliente con los calculados.
// El total siempre se compara. Con descuento o recargo, subtotal e IVA no se
// comparan (la impresora aplica el ajuste sobre el total y el desglose puede
// variar); sin ellos, se comparan si vienen informados.
func checkTotals(want, got invoiceTotals) error {
	if math.Abs(want.Total-got.Total) > totalsTolerance {
		return fmt.Errorf("totals.total %.2f no coincide con el calculado %.2f", want.Total, got.Total)
	}
	if got.Discount != 0 || got.Surcharge != 0 {
		return nil
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

// cleanField valida un campo de texto libre del protocolo (cliente, medio de
// pago): ASCII imprimible, sin `|` ni CR/LF, con ancho máximo.
func cleanField(s string, max int, label string) (string, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return "", nil
	}
	if len(v) > max {
		return "", fmt.Errorf("%s excede %d caracteres", label, max)
	}
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			return "", fmt.Errorf("%s debe ser ASCII imprimible (sin acentos ni saltos de línea)", label)
		}
		if r == '|' {
			return "", fmt.Errorf("%s no puede contener '|'", label)
		}
	}
	return v, nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
