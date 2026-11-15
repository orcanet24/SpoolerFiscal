package fiscal

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"spoolerfiscal/internal/frame"
)

// Comandos ASCII del protocolo TFHKA (The Factory HKA) usados por el spooler.
// El protocolo completo está documentado en docs/PROTOCOLO_TFHKA.md.
const (
	CmdStatusS2 = "S2"
	CmdInfoS1   = "S1"
	CmdInfoS5   = "S5"
)

var (
	// "0|Factura 12|Total: 116.00|Impreso:3"
	tfhkaDocRe = regexp.MustCompile(`^(?:0\|)?(Factura|Nota de Credito|Nota de Debito|Doc No Fiscal)\s+([0-9]+)\|(?:Total:\s*([^|]*))?`)
	// Respuestas con bloque de estado tipo "0x04|0x00|...".
	tfhkaStatusPrefixRe = regexp.MustCompile(`^0x([0-9A-Fa-f]{2})\|0x([0-9A-Fa-f]{2})\|`)
	// "ERROR|0x50|Comando invalido"
	tfhkaErrorRe = regexp.MustCompile(`^ERROR\|0x([0-9A-Fa-f]{2})\|?(.*)$`)
)

// TFHKA implementa Printer sobre el protocolo ASCII de The Factory HKA.
type TFHKA struct {
	opts Options

	mu         sync.Mutex
	tr         frame.Stream
	last       Status
	info       Info
	infoLoaded bool
	now        func() time.Time
}

func newTFHKA(opts Options) *TFHKA {
	opts = opts.normalized()
	return &TFHKA{opts: opts, now: time.Now}
}

// Driver identifica la implementación.
func (p *TFHKA) Driver() string { return BrandTFHKA }

// Step clasifica el comando según el protocolo TFHKA: la primera línea de ítem
// abre la factura (prefijo de alícuota), 101/81 la cierran y los documentos no
// fiscales se abren con 80$ y se cierran con 81.
func (p *TFHKA) Step(cmd []byte) Step {
	raw := strings.TrimRight(string(cmd), "\r\n")
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return StepNeutral
	}
	switch trimmed {
	case "101", "81":
		return StepCloseDocument
	}
	if strings.HasPrefix(trimmed, "80$") || strings.HasPrefix(trimmed, "d1") ||
		strings.HasPrefix(trimmed, "d2") || strings.HasPrefix(trimmed, "`") {
		return StepOpenDocument
	}
	// Consultas y reportes son neutrales aunque traigan espacios de más.
	switch trimmed[0] {
	case 'S', 'U', '9':
		return StepNeutral
	}
	// El prefijo de alícuota es el primer carácter: espacio = exento.
	switch raw[0] {
	case '!', '"', '#', ' ':
		return StepOpenDocument
	}
	return StepNeutral
}

// Open abre el transporte exclusivo hacia la impresora.
func (p *TFHKA) Open(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tr != nil {
		return nil
	}
	if p.opts.NewTransport == nil {
		return errors.New("tfhka: falta la fábrica de transporte")
	}
	tr, err := p.opts.NewTransport()
	if err != nil {
		p.last.Online = false
		p.last.LastError = err.Error()
		p.last.Checked = p.now()
		return err
	}
	p.tr = tr
	p.last.Online = true
	p.last.LastError = ""
	p.last.Checked = p.now()
	p.opts.Logger.Info("impresora fiscal abierta", "driver", BrandTFHKA, "puerto", p.opts.PortLabel)
	return nil
}

// Close libera el puerto.
func (p *TFHKA) Close() error {
	p.mu.Lock()
	tr := p.tr
	p.tr = nil
	p.last.Online = false
	p.mu.Unlock()
	if tr == nil {
		return nil
	}
	return tr.Close()
}

func (p *TFHKA) transport() frame.Stream {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tr
}

// SendCommand escribe los bytes del comando tal cual, sin interpretarlos.
func (p *TFHKA) SendCommand(ctx context.Context, cmd []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tr := p.transport()
	if tr == nil {
		return ErrNotOpen
	}
	if err := frame.WriteSettled(tr, cmd, p.opts.WriteSettle); err != nil {
		p.setOffline(err)
		return fmt.Errorf("escribir en impresora: %w", err)
	}
	return nil
}

// ReadResponse lee una respuesta completa hasta el silencio del puerto.
func (p *TFHKA) ReadResponse(ctx context.Context) ([]byte, error) {
	tr := p.transport()
	if tr == nil {
		return nil, ErrNotOpen
	}
	resp, err := frame.Read(ctx, tr, p.opts.ReadGap, p.opts.ResponseTimeout, p.opts.MaxResponseBytes)
	if err != nil {
		p.setOffline(err)
		if errors.Is(err, frame.ErrTimeout) {
			return nil, ErrNoResponse
		}
		return nil, err
	}
	p.mu.Lock()
	p.last.Online = true
	p.last.LastError = ""
	p.last.Checked = p.now()
	p.mu.Unlock()
	return resp, nil
}

// RoundTrip envía un comando interno y devuelve la respuesta.
func (p *TFHKA) RoundTrip(ctx context.Context, cmd string) ([]byte, error) {
	payload := []byte(cmd + p.opts.CommandEnding)
	if err := p.SendCommand(ctx, payload); err != nil {
		return nil, err
	}
	return p.ReadResponse(ctx)
}

// GetStatus consulta el estado con el comando sonda configurado.
func (p *TFHKA) GetStatus(ctx context.Context) (Status, error) {
	probe := strings.TrimSpace(p.opts.StatusProbe)
	if probe == "" {
		return p.cached(), nil
	}
	resp, err := p.RoundTrip(ctx, probe)
	if err != nil {
		st := p.cached()
		st.Online = false
		st.LastError = err.Error()
		st.Checked = p.now()
		p.mu.Lock()
		p.last = st
		p.mu.Unlock()
		return st, err
	}
	return p.Describe([]byte(probe), resp).Status, nil
}

// GetInfo consulta identificación (S1, cacheada) y memoria fiscal (S5, siempre
// viva). La identificación no cambia durante la operación; la memoria libre sí,
// así que S5 se consulta en cada llamada para que el monitoreo sea real.
func (p *TFHKA) GetInfo(ctx context.Context) (Info, error) {
	info, identityErr := p.identity(ctx)

	// S5: memoria fiscal, siempre fresca.
	if resp, err := p.RoundTrip(ctx, CmdInfoS5); err != nil {
		if identityErr == nil && info.Serial != "" {
			return info, fmt.Errorf("tfhka: memoria fiscal (S5): %w", err)
		}
		if identityErr != nil {
			return info, identityErr
		}
		return info, fmt.Errorf("tfhka: memoria fiscal (S5): %w", err)
	} else {
		parseTFHKAS5(string(resp), &info)
	}

	p.mu.Lock()
	p.info = info
	p.mu.Unlock()
	return info, identityErr
}

// identity obtiene la identificación por S1, cacheada tras la primera consulta
// exitosa (serial, RIF y últimos números no cambian sin un corte de energía).
func (p *TFHKA) identity(ctx context.Context) (Info, error) {
	p.mu.Lock()
	if p.infoLoaded {
		info := p.info
		p.mu.Unlock()
		return info, nil
	}
	info := p.info
	p.mu.Unlock()

	info.Brand = BrandTFHKA
	info.Model = "TFHKA"
	info.Port = p.opts.PortLabel
	info.Firmware = "n/d"

	resp, err := p.RoundTrip(ctx, CmdInfoS1)
	if err != nil {
		return info, err
	}
	parseTFHKAS1(string(resp), &info)
	if info.Serial == "" && info.RIF == "" {
		return info, errors.New("tfhka: la impresora no reportó identificación")
	}

	p.mu.Lock()
	p.info = info
	p.infoLoaded = true
	p.mu.Unlock()
	return info, nil
}

// GetCorrelative implementa CorrelativeSource: S1 es la fuente legal de los
// últimos números emitidos (facturas, NC, ND). Barato: un solo round-trip.
func (p *TFHKA) GetCorrelative(ctx context.Context) (Info, error) {
	info := Info{Brand: BrandTFHKA, Port: p.opts.PortLabel}
	resp, err := p.RoundTrip(ctx, CmdInfoS1)
	if err != nil {
		return info, err
	}
	parseTFHKAS1(string(resp), &info)
	if info.Serial == "" && info.RIF == "" {
		return info, errors.New("tfhka: S1 sin identificación")
	}
	return info, nil
}

// Describe interpreta una respuesta sin modificarla.
func (p *TFHKA) Describe(cmd, resp []byte) Description {
	text := strings.TrimSpace(string(resp))
	d := Description{}

	p.mu.Lock()
	st := p.last
	p.mu.Unlock()

	st.Online = len(resp) > 0
	st.Checked = p.now()
	st.Raw = text

	hadStatus := false
	if m := tfhkaStatusPrefixRe.FindStringSubmatch(text); m != nil {
		hadStatus = true
		sb := hexByte(m[1])
		eb := hexByte(m[2])
		st.StatusByte, st.ErrorByte = sb, eb
		st.StatusText = statusText(sb)
		st.ErrorText = errorText(eb)
		st.PaperOut = eb == 0x01 || eb == 0x02 || eb == 0x03
		st.MemoryFull = eb == 0x6C || sb == 0x0A || sb == 0x0B || sb == 0x0C
		st.MemoryAlmostFull = sb == 0x07 || sb == 0x08 || sb == 0x09
		st.DocumentOpen = sb >= 0x02 && sb <= 0x0C && sb != 0x04 && sb != 0x07 && sb != 0x0A
		text = strings.TrimPrefix(text, m[0])
	}

	if m := tfhkaErrorRe.FindStringSubmatch(text); m != nil {
		st.ErrorByte = hexByte(m[1])
		st.ErrorText = strings.TrimSpace(m[2])
		if st.ErrorText == "" {
			st.ErrorText = errorText(st.ErrorByte)
		}
		st.PaperOut = st.ErrorByte == 0x01 || st.ErrorByte == 0x02 || st.ErrorByte == 0x03
		st.MemoryFull = st.ErrorByte == 0x6C
		st.StatusText = statusText(st.StatusByte)
		st.LastError = st.ErrorText
		d.Failed = true
		d.ErrorCode = st.ErrorByte
		d.ErrorText = st.ErrorText
		d.Status = st
		p.mu.Lock()
		p.last = st
		p.mu.Unlock()
		return d
	}

	if m := tfhkaDocRe.FindStringSubmatch(text); m != nil {
		num, _ := strconv.ParseInt(m[2], 10, 64)
		d.Document = &Document{
			Type:   DocTypeFromLabel(m[1]),
			Number: num,
			Total:  strings.TrimSpace(m[3]),
		}
	}
	// Sin bloque de estado en esta respuesta no se arrastran flags viejos: la
	// impresora no informó nada, así que el estado queda "no reportado".
	if !hadStatus {
		st.StatusByte, st.ErrorByte = 0, 0
		st.StatusText, st.ErrorText = "", ""
		st.PaperOut, st.MemoryFull, st.MemoryAlmostFull, st.DocumentOpen = false, false, false, false
	}
	d.Status = st
	p.mu.Lock()
	p.last = st
	p.mu.Unlock()
	return d
}

func (p *TFHKA) cached() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *TFHKA) setOffline(err error) {
	p.mu.Lock()
	p.last.Online = false
	p.last.LastError = err.Error()
	p.last.Checked = p.now()
	p.mu.Unlock()
}

func hexByte(s string) byte {
	v, err := strconv.ParseUint(s, 16, 8)
	if err != nil {
		return 0
	}
	return byte(v)
}

// parseTFHKAS1 interpreta "0|maquina|rif|auditoria|corte|ultFact|ultNC|ultND|ultDNF|ventas|fecha".
func parseTFHKAS1(text string, info *Info) {
	fields := strings.Split(strings.TrimSpace(text), "|")
	if len(fields) > 0 && fields[0] == "0" {
		fields = fields[1:]
	}
	if len(fields) >= 2 {
		if info.Serial == "" {
			info.Serial = fields[0]
		}
		if info.RIF == "" {
			info.RIF = strings.ReplaceAll(fields[1], "*", "")
		}
	}
	if len(fields) >= 7 {
		info.LastInvoice = atoi(fields[4])
		info.LastCredit = atoi(fields[5])
		info.LastDebit = atoi(fields[6])
	}
}

// parseTFHKAS5 interpreta "maquina|rif|nroAuditoria|totalMB|libreMB|docs".
func parseTFHKAS5(text string, info *Info) {
	fields := strings.Split(strings.TrimSpace(text), "|")
	if len(fields) >= 2 {
		if info.Serial == "" {
			info.Serial = strings.ReplaceAll(fields[0], "*", "")
		}
		if info.RIF == "" {
			info.RIF = strings.ReplaceAll(fields[1], "*", "")
		}
	}
	if len(fields) >= 5 {
		info.MemoryTotalMB = atof(fields[3])
		info.MemoryFreeMB = atof(fields[4])
	}
}

func atoi(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(strings.ReplaceAll(s, "*", "")), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func atof(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(s, "*", "")), 64)
	if err != nil {
		return 0
	}
	return v
}

// statusText y errorText siguen las tablas Anexo 1 y Anexo 2 del protocolo.
func statusText(b byte) string {
	if t, ok := tfhkaStatusText[b]; ok {
		return t
	}
	return fmt.Sprintf("Estado 0x%02X", b)
}

func errorText(b byte) string {
	if t, ok := tfhkaErrorText[b]; ok {
		return t
	}
	return fmt.Sprintf("Error 0x%02X", b)
}

var tfhkaStatusText = map[byte]string{
	0x00: "Estado desconocido",
	0x01: "Modo prueba - en espera",
	0x02: "Modo prueba - emisión fiscal",
	0x03: "Modo prueba - emisión no fiscal",
	0x04: "Modo fiscal - en espera",
	0x05: "Modo fiscal - emisión fiscal",
	0x06: "Modo fiscal - emisión no fiscal",
	0x07: "Memoria fiscal casi llena - en espera",
	0x08: "Memoria fiscal casi llena - emisión fiscal",
	0x09: "Memoria fiscal casi llena - emisión no fiscal",
	0x0A: "Memoria fiscal llena - en espera",
	0x0B: "Memoria fiscal llena - emisión fiscal",
	0x0C: "Memoria fiscal llena - emisión no fiscal",
}

var tfhkaErrorText = map[byte]string{
	0x00: "Sin error",
	0x01: "Fin de papel",
	0x02: "Error mecánico de papel",
	0x03: "Fin de papel + error mecánico",
	0x50: "Comando inválido",
	0x54: "Tasa inválida",
	0x58: "Sin directivas",
	0x5C: "Comando inválido",
	0x60: "Error fiscal",
	0x64: "Error de memoria fiscal",
	0x6C: "Memoria fiscal llena",
	0x70: "Buffer completo - reiniciar",
	0x80: "Error de comunicación",
	0x89: "Sin respuesta",
	0x90: "Error de LRC",
	0x91: "Error interno de la API",
	0x99: "Error al abrir archivo",
}
