// Package fiscal abstrae la impresora fiscal: transporte serie, protocolo,
// estado y detección de documentos emitidos. El resto del sistema sólo usa la
// interfaz Printer, de modo que agregar otra marca es implementar esa interfaz.
package fiscal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"spoolerfiscal/internal/frame"
)

// Marcas y tipos de documento.
const (
	BrandTFHKA  = "tfhka"
	DocInvoice  = "invoice"
	DocCredit   = "credit_note"
	DocDebit    = "debit_note"
	DocNonFisc  = "non_fiscal"
	DocUnknown  = "unknown"
	ProtocolVer = "1.0"
)

var (
	// ErrNotOpen indica que la impresora no tiene transporte abierto.
	ErrNotOpen = errors.New("fiscal: impresora no abierta")
	// ErrNoResponse indica que el comando salió pero no hubo respuesta.
	ErrNoResponse = errors.New("fiscal: la impresora no respondió (posible comando impreso sin confirmación)")
)

// Document es un documento fiscal emitido, según lo reportado por la impresora.
type Document struct {
	Type   string
	Number int64
	Total  string
}

// Status es el estado reportado por la impresora.
type Status struct {
	Online           bool
	StatusByte       byte
	ErrorByte        byte
	StatusText       string
	ErrorText        string
	PaperOut         bool
	MemoryFull       bool
	MemoryAlmostFull bool
	DocumentOpen     bool
	LastError        string
	Raw              string
	Checked          time.Time
}

// Info son los datos de identificación y memoria fiscal de la impresora.
type Info struct {
	Brand         string
	Model         string
	Serial        string
	Firmware      string
	RIF           string
	Port          string
	MemoryTotalMB float64
	MemoryFreeMB  float64
	LastInvoice   int64
	LastCredit    int64
	LastDebit     int64
}

// Description es la interpretación de una respuesta del protocolo. El
// contenido de la respuesta nunca se altera: sólo se describe.
type Description struct {
	Failed    bool
	ErrorCode byte
	ErrorText string
	Document  *Document
	Status    Status
}

// Step clasifica el efecto de un comando sobre el documento en curso.
type Step int

const (
	// StepNeutral no abre ni cierra documento (pagos, subtotales, reportes).
	StepNeutral Step = iota
	// StepOpenDocument abre o continúa un documento fiscal.
	StepOpenDocument
	// StepCloseDocument cierra el documento fiscal en curso.
	StepCloseDocument
)

// Printer es la interfaz que exige el spooler. Open/Close administran el
// acceso exclusivo al puerto, SendCommand/ReadResponse transportan bytes y
// GetStatus/GetInfo consultan la impresora.
type Printer interface {
	Open(ctx context.Context) error
	Close() error
	SendCommand(ctx context.Context, cmd []byte) error
	ReadResponse(ctx context.Context) ([]byte, error)
	GetStatus(ctx context.Context) (Status, error)
	GetInfo(ctx context.Context) (Info, error)
	// Describe interpreta una respuesta ya recibida (documento, estado, error).
	Describe(cmd, resp []byte) Description
	// Step indica si el comando abre o cierra un documento. Permite que el
	// spooler le dé turno exclusivo a una estación mientras factura.
	Step(cmd []byte) Step
	Driver() string
}

// Options configura la capa fiscal.
type Options struct {
	NewTransport     func() (frame.Stream, error)
	ReadGap          time.Duration
	ResponseTimeout  time.Duration
	MaxResponseBytes int
	WriteSettle      time.Duration
	StatusProbe      string
	CommandEnding    string
	PortLabel        string
	Logger           *slog.Logger
}

func (o Options) normalized() Options {
	if o.ReadGap <= 0 {
		o.ReadGap = 30 * time.Millisecond
	}
	if o.ResponseTimeout <= 0 {
		o.ResponseTimeout = 20 * time.Second
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = 32 * 1024
	}
	if o.CommandEnding == "" {
		o.CommandEnding = "\r"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// CorrelativeSource es una capacidad opcional del driver: consulta la memoria
// fiscal por los últimos números emitidos (fuente legal del correlativo) sin
// re-consultar toda la identificación. El spooler la usa para conciliar su
// correlativo con la realidad de la impresora antes y después de cada factura.
// Los drivers que no lo implementan caen en GetInfo.
type CorrelativeSource interface {
	GetCorrelative(ctx context.Context) (Info, error)
}

// DocumentCloser es una capacidad opcional del driver: indica el comando con
// el que se cierra por recuperación un documento que la impresora dejó abierto
// (factura colgada por un corte). TFHKA usa "101"; IxBatch usa "@Close". Sin
// esta interfaz el spooler asume el cierre de TFHKA.
type DocumentCloser interface {
	CloseDocumentCommand() []byte
}

// NormalizeCommandTerminator ajusta el terminador de comandos que exige el
// protocolo del driver. IxBatch delimita por línea: un CR suelto no cierra el
// comando y la impresora espera hasta el timeout. En TFHKA el terminador es
// configurable y se respeta tal cual.
func NormalizeCommandTerminator(driver, terminator string) string {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case BrandHasar, BrandBixolon, BrandEpson:
		if !strings.Contains(terminator, "\n") {
			return "\r\n"
		}
	}
	return terminator
}

// driverRegistry permite soportar varias marcas de impresoras fiscales. Cada
// marca implementa la interfaz Printer (y opcionalmente CorrelativeSource) y se
// registra aquí; la configuración elige por nombre.
//
// Familias implementadas:
//   - tfhka: protocolo ASCII de The Factory HKA.
//   - hasar, bixolon, epson: protocolo IxBatch (@Comando|param...); Epson
//     antepone "0xESTADO|0xERROR|" a cada respuesta. Comparten la misma
//     implementación (IxBatch) y sólo cambia el nombre del modelo/envoltorio.
//
// Otras marcas se agregan con RegisterDriver cuando su protocolo esté
// verificado contra hardware o contra una referencia byte a byte.
type driverFactory func(Options) (Printer, error)

var (
	driverMu       sync.RWMutex
	driverRegistry = map[string]driverFactory{
		BrandTFHKA:   func(o Options) (Printer, error) { return newTFHKA(o), nil },
		BrandHasar:   func(o Options) (Printer, error) { return newIxBatch(BrandHasar, o), nil },
		BrandBixolon: func(o Options) (Printer, error) { return newIxBatch(BrandBixolon, o), nil },
		BrandEpson:   func(o Options) (Printer, error) { return newIxBatch(BrandEpson, o), nil },
	}
)

// RegisterDriver agrega una marca de impresora al registro. Devuelve error si
// el nombre está vacío o ya existe.
func RegisterDriver(name string, factory driverFactory) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return errors.New("fiscal: nombre de driver vacío")
	}
	if factory == nil {
		return fmt.Errorf("fiscal: fábrica de driver %q vacía", name)
	}
	driverMu.Lock()
	defer driverMu.Unlock()
	if _, dup := driverRegistry[name]; dup {
		return fmt.Errorf("fiscal: driver %q ya registrado", name)
	}
	driverRegistry[name] = factory
	return nil
}

// Drivers lista las marcas registradas.
func Drivers() []string {
	driverMu.RLock()
	defer driverMu.RUnlock()
	out := make([]string, 0, len(driverRegistry))
	for name := range driverRegistry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// New construye el driver indicado. Cadena vacía = driver por omisión (TFHKA).
func New(driver string, opts Options) (Printer, error) {
	name := strings.ToLower(strings.TrimSpace(driver))
	if name == "" {
		name = BrandTFHKA
	}
	driverMu.RLock()
	factory, ok := driverRegistry[name]
	driverMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fiscal: driver %q no soportado (soportados: %s)", driver, strings.Join(Drivers(), ", "))
	}
	return factory(opts)
}

// DocTypeFromLabel traduce la etiqueta del protocolo al tipo interno.
func DocTypeFromLabel(label string) string {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "factura":
		return DocInvoice
	case "nota de credito", "nota de crédito":
		return DocCredit
	case "nota de debito", "nota de débito":
		return DocDebit
	case "doc no fiscal", "documento no fiscal":
		return DocNonFisc
	default:
		return DocUnknown
	}
}
