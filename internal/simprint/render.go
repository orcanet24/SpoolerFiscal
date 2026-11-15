package simprint

import (
	"fmt"
	"strings"
	"time"
)

// Width es el ancho típico de una térmica de 80 mm en 12 columnas por pulgada
// (42 caracteres). ESC/POS y el texto usan el mismo ancho.
const Width = 42

var rules = strings.Repeat("-", Width)

func center(s string) string {
	s = truncate(s, Width)
	pad := (Width - len([]rune(s))) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + s
}

// Truncate corta la cadena a n bytes (ASCII puro, 1 byte = 1 carácter).
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func truncate(s string, n int) string { return Truncate(s, n) }

// Title traduce el tipo de documento al encabezado del ticket.
func Title(doc Document) string {
	switch doc.Type {
	case "invoice":
		return "FACTURA"
	case "credit":
		return "NOTA DE CREDITO"
	case "debit":
		return "NOTA DE DEBITO"
	case "nonfiscal":
		return "DOC. NO FISCAL"
	case "report":
		return "REPORTE X"
	case "zreport":
		return "CIERRE Z"
	}
	return "DOCUMENTO"
}

// RenderText devuelve las líneas del ticket en texto plano (42 columnas): la
// vista previa de lo que ESC/POS imprime en la térmica.
func RenderText(doc Document, cfg Config) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(truncate(s, Width) + "\n") }

	if doc.Type == "report" || doc.Type == "zreport" {
		line(rules)
		line(center(cfg.Store))
		line(center(Title(doc)))
		line(rules)
		line(fmt.Sprintf("No: %d", doc.Number))
		line(fmt.Sprintf("Fecha: %s", doc.At.Format("2006-01-02 15:04:05")))
		line(rules)
		line(fmt.Sprintf("VENTAS GRAVADAS:  %12.2f", doc.Base))
		line(fmt.Sprintf("IVA:              %12.2f", doc.Tax))
		line(fmt.Sprintf("VENTAS TOTALES:   %12.2f", doc.Total))
		line(rules)
		line(center("*** DOCUMENTO DE PRUEBA - NO FISCAL ***"))
		line(center("Simulador SpoolerFiscal"))
		line(rules)
		return b.String()
	}

	line(center(cfg.Store))
	line(center("RIF: " + cfg.RIF))
	line(center("Serial: " + cfg.SerialID))
	line(rules)
	line(center(fmt.Sprintf("%s No %d", Title(doc), doc.Number)))
	line(fmt.Sprintf("Fecha: %s", doc.At.Format("2006-01-02 15:04:05")))
	line(rules)
	for _, it := range doc.Items {
		line(truncate(it.Desc, Width))
		line(fmt.Sprintf("  %7.3f x %9.2f %20.2f", it.Qty, it.Price, round2(it.Qty*it.Price)))
	}
	line(rules)
	rate := 0.0
	if doc.Base > 0 {
		rate = round2(doc.Tax / doc.Base * 100)
	}
	line(fmt.Sprintf("BASE IMPONIBLE:    %12.2f", doc.Base))
	if doc.Tax > 0 {
		line(fmt.Sprintf("IVA %.0f%%:           %12.2f", rate, doc.Tax))
	}
	line(fmt.Sprintf("TOTAL:             %12.2f", doc.Total))
	line(rules)
	line(center("*** DOCUMENTO DE PRUEBA - NO FISCAL ***"))
	line(center("Simulador SpoolerFiscal"))
	line(rules)
	return b.String()
}

// RFC3339 corto para el encabezado ESC/POS.
func stamp(t time.Time) string { return t.Format("02-01-2006 15:04") }
