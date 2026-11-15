// Package escpos genera la secuencia de bytes ESC/POS que imprime un ticket
// en una impresora térmica común de 80 mm (USB/serial en modo COM o TCP en el
// puerto 9100). ASCII puro: sin code pages ni imágenes.
package escpos

import (
	"fmt"
	"strconv"
	"strings"

	"spoolerfiscal/internal/simprint"
)

// Comandos ESC/POS usados.
const (
	initP    = "\x1b@"     // reinicia la impresora
	alignL   = "\x1ba\x00" // alineación izquierda
	alignC   = "\x1ba\x01" // alineación centrada
	boldOn   = "\x1bE\x01" // énfasis
	boldOff  = "\x1bE\x00"
	normal   = "\x1b!\x00"           // fuente normal
	doubleHi = "\x1b!\x30"           // doble alto y ancho (totales)
	cutP     = "\n\n\n\x1dV\x42\x00" // avance + corte
)

// ascii limpia la cadena a ASCII imprimible: las térmicas en este modo
// impresor no tienen acentos.
func ascii(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == 'á':
			return 'a'
		case r == 'é':
			return 'e'
		case r == 'í':
			return 'i'
		case r == 'ó':
			return 'o'
		case r == 'ú':
			return 'u'
		case r == 'ñ':
			return 'n'
		case r == 'Á':
			return 'A'
		case r == 'É':
			return 'E'
		case r == 'Í':
			return 'I'
		case r == 'Ó':
			return 'O'
		case r == 'Ú':
			return 'U'
		case r == 'Ñ':
			return 'N'
		case r >= 0x20 && r <= 0x7e:
			return r
		default:
			return ' '
		}
	}, s)
	return strings.TrimRight(s, " ")
}

func l(s string) string { return ascii(s) + "\r\n" }

// Ticket genera los bytes ESC/POS completos de un documento del emulador,
// con su corte de papel al final.
func Ticket(doc simprint.Document, cfg simprint.Config) []byte {
	var b strings.Builder
	b.WriteString(initP)

	if doc.Type == "report" || doc.Type == "zreport" {
		b.WriteString(alignC + boldOn + l(cfg.Store) + l(simprint.Title(doc)) + boldOff)
		b.WriteString(alignL)
		b.WriteString(l("No: " + strconv.FormatInt(doc.Number, 10)))
		b.WriteString(l("Fecha: " + doc.At.Format("2006-01-02 15:04:05")))
		b.WriteString(l(strings.Repeat("-", simprint.Width)))
		b.WriteString(fmt.Sprintf("%-18s%12.2f\r\n", "VENTAS GRAVADAS:", doc.Base))
		b.WriteString(fmt.Sprintf("%-18s%12.2f\r\n", "IVA:", doc.Tax))
		b.WriteString(fmt.Sprintf("%-18s%12.2f\r\n", "VENTAS TOTALES:", doc.Total))
		b.WriteString(l(strings.Repeat("-", simprint.Width)))
		writeFooter(&b)
		return []byte(b.String() + cutP)
	}

	// Encabezado
	b.WriteString(alignC + boldOn + l(cfg.Store) + boldOff)
	b.WriteString(alignL)
	b.WriteString(l("RIF: " + cfg.RIF))
	b.WriteString(l("Serial: " + cfg.SerialID))
	b.WriteString(l(strings.Repeat("-", simprint.Width)))
	b.WriteString(alignC + boldOn + l(simprint.Title(doc)+" No "+strconv.FormatInt(doc.Number, 10)) + boldOff)
	b.WriteString(alignL)
	b.WriteString(l("Fecha: " + doc.At.Format("02-01-2006 15:04")))
	b.WriteString(l(strings.Repeat("-", simprint.Width)))

	// Ítems
	for _, it := range doc.Items {
		b.WriteString(l(simprint.Truncate(it.Desc, simprint.Width)))
		line := fmt.Sprintf("%7.3f x %9.2f %20.2f", it.Qty, it.Price, it.Qty*it.Price)
		b.WriteString(l("  " + line))
	}
	b.WriteString(l(strings.Repeat("-", simprint.Width)))

	// Totales
	b.WriteString(fmt.Sprintf("%-20s%13.2f\r\n", "BASE IMPONIBLE:", doc.Base))
	if doc.Tax > 0 {
		b.WriteString(fmt.Sprintf("%-20s%13.2f\r\n", "IVA:", doc.Tax))
	}
	b.WriteString(doubleHi + fmt.Sprintf("%-14s%12.2f\r\n", "TOTAL:", doc.Total) + normal)

	writeFooter(&b)
	return []byte(b.String() + cutP)
}

func writeFooter(b *strings.Builder) {
	b.WriteString(l(strings.Repeat("-", simprint.Width)))
	b.WriteString(alignC)
	b.WriteString(l("*** DOCUMENTO DE PRUEBA - NO FISCAL ***"))
	b.WriteString(l("Simulador SpoolerFiscal"))
	b.WriteString(alignL)
}
