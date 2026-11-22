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

// Marcas que hablan el protocolo IxBatch (@Comando|param...). Comparten el
// conjunto de comandos y se diferencian en el envoltorio de la respuesta:
// Epson antepone "0xESTADO|0xERROR|" a cada respuesta. La misma
// implementación sirve para las tres; el nombre del driver elige el modelo y
// el envoltorio.
const (
	BrandHasar   = "hasar"
	BrandBixolon = "bixolon"
	BrandEpson   = "epson"

	// CmdIxStatus es la sonda de estado del protocolo IxBatch.
	CmdIxStatus = "@Status"
)

var ixBatchModels = map[string]string{
	BrandHasar:   "SMH/P-615F",
	BrandBixolon: "SRP-270",
	BrandEpson:   "TM2000",
}

var (
	// "0x04|0x00|@Response|OK|0|..." (sólo Epson).
	ixEnvelopeRe = regexp.MustCompile(`^0x([0-9A-Fa-f]{2})\|0x([0-9A-Fa-f]{2})\|`)
	// "@Response|OK|0|cuerpo" o "@Response|Error|0x50|motivo".
	// El tercer campo (código) puede ser decimal (2) o hexadecimal (0x50).
	ixResponseRe = regexp.MustCompile(`^@Response\|([A-Za-z]+)\|([^|]*)\|?`)
	// El bloque de estado sólo se interpreta en la sonda @Status, anclado al
	// inicio del cuerpo: una descripción de ítem no puede falsificarlo.
	ixStatusRe   = regexp.MustCompile(`^Status:\s*0x([0-9A-Fa-f]{2})`)
	ixErrorRe    = regexp.MustCompile(`(?:^|\|)Error:\s*0x([0-9A-Fa-f]{2})`)
	ixDocStateRe = regexp.MustCompile(`(?:^|\|)Doc:\s*([A-Za-zÁÉÍÓÚáéíóú]+)`)
	// "Factura 12 cerrada|Total: 116.00|Impreso:3". Se exige "cerrada": es la
	// forma en que el protocolo confirma el documento y evita confundir una
	// línea de ítem ("Factura 12") con un documento emitido.
	ixDocRe   = regexp.MustCompile(`^(Factura|Nota de Credito|Nota de Débito|Nota de Debito|Documento)\s+([0-9]+)\s+cerrada\b`)
	ixTotalRe = regexp.MustCompile(`Total:\s*([^|]+)`)
)

// IxBatch implementa Printer sobre el protocolo IxBatch de Hasar SMH/P-615F,
// Bixolon SRP-270 y Epson TM2000. El spooler transporta bytes: este driver
// sólo clasifica comandos e interpreta respuestas.
type IxBatch struct {
	brand string
	model string
	opts  Options

	mu   sync.Mutex
	tr   frame.Stream
	last Status
	info Info
	now  func() time.Time
}

func newIxBatch(brand string, opts Options) *IxBatch {
	opts = opts.normalized()
	brand = strings.ToLower(strings.TrimSpace(brand))
	// El protocolo IxBatch delimita por línea: un CR suelto no cierra el
	// comando y la impresora se queda esperando. Se fuerza CRLF y se avisa.
	if fixed := NormalizeCommandTerminator(brand, opts.CommandEnding); fixed != opts.CommandEnding {
		opts.Logger.Warn("command_terminator sin LF: IxBatch exige CRLF; se usa \\r\\n",
			"driver", brand, "configurado", strconv.Quote(opts.CommandEnding))
		opts.CommandEnding = fixed
	}
	// La sonda por omisión de la configuración es S2 (TFHKA). En IxBatch la
	// consulta de estado es @Status.
	if probe := strings.TrimSpace(opts.StatusProbe); probe == "" || !strings.HasPrefix(probe, "@") {
		opts.StatusProbe = CmdIxStatus
	}
	return &IxBatch{
		brand: brand, model: ixBatchModels[brand], opts: opts, now: time.Now,
		info: Info{Brand: brand, Model: ixBatchModels[brand], Port: opts.PortLabel, Firmware: "n/d"},
	}
}

// Driver identifica la marca implementada.
func (p *IxBatch) Driver() string { return p.brand }

// CloseDocumentCommand implementa DocumentCloser: en IxBatch el cierre de
// recuperación de un documento abierto es @Close.
func (p *IxBatch) CloseDocumentCommand() []byte { return []byte("@Close") }

// Step clasifica el comando IxBatch según su efecto sobre el documento en
// curso. Las líneas de ítem se tratan como apertura para que el turno
// exclusivo se retenga aunque el emisor no haya mandado @OpenFiscalReceipt
// explícito: entremezclar documentos fiscales es peor que retener un turno
// que se libera por timeout.
func (p *IxBatch) Step(cmd []byte) Step {
	switch ixCommandName(cmd) {
	case "@openfiscalreceipt", "@opencreditnote", "@opendebitnote", "@opennonfiscaldoc",
		"@printlineitem", "@printline", "@refunditem":
		return StepOpenDocument
	case "@close", "@closefiscalreceipt", "@closecashreceipt", "@closenonfiscaldoc",
		"@cancel", "@refundclose":
		return StepCloseDocument
	}
	return StepNeutral
}

// Open abre el transporte exclusivo hacia la impresora.
func (p *IxBatch) Open(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tr != nil {
		return nil
	}
	if p.opts.NewTransport == nil {
		return errors.New("ixbatch: falta la fábrica de transporte")
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
	p.opts.Logger.Info("impresora fiscal abierta", "driver", p.brand, "puerto", p.opts.PortLabel)
	return nil
}

// Close libera el puerto.
func (p *IxBatch) Close() error {
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

func (p *IxBatch) transport() frame.Stream {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tr
}

// SendCommand escribe los bytes del comando tal cual, sin interpretarlos.
func (p *IxBatch) SendCommand(ctx context.Context, cmd []byte) error {
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
func (p *IxBatch) ReadResponse(ctx context.Context) ([]byte, error) {
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
func (p *IxBatch) RoundTrip(ctx context.Context, cmd string) ([]byte, error) {
	payload := []byte(cmd + p.opts.CommandEnding)
	if err := p.SendCommand(ctx, payload); err != nil {
		return nil, err
	}
	return p.ReadResponse(ctx)
}

// GetStatus consulta el estado con la sonda configurada (@Status por omisión).
func (p *IxBatch) GetStatus(ctx context.Context) (Status, error) {
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

// GetInfo devuelve la identificación del modelo. El subconjunto IxBatch
// implementado por el simulador de referencia no expone serial ni memoria
// fiscal, así que se sondea el estado para confirmar que la impresora responde
// y se devuelven los datos que sí se conocen por configuración.
func (p *IxBatch) GetInfo(ctx context.Context) (Info, error) {
	info := p.infoOr()
	if _, err := p.GetStatus(ctx); err != nil {
		return info, err
	}
	return info, nil
}

func (p *IxBatch) infoOr() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := p.info
	info.Brand = p.brand
	if info.Model == "" {
		info.Model = p.model
	}
	info.Port = p.opts.PortLabel
	return info
}

// Describe interpreta una respuesta sin modificarla. Acepta el envoltorio de
// Epson, los errores "@Response|Error|código|motivo", el bloque de estado de
// @Status y el cierre de documento con su número.
//
// El estado y el documento sólo se interpretan en los comandos que los
// reportan (@Status y los cierres): el cuerpo de un ítem lo escribe el usuario
// y no debe poder fabricar un documento ni un estado.
func (p *IxBatch) Describe(cmd, resp []byte) Description {
	text := strings.TrimSpace(string(resp))
	d := Description{}

	p.mu.Lock()
	st := p.last
	p.mu.Unlock()

	st.Online = len(resp) > 0
	st.Checked = p.now()
	st.Raw = text

	name := ixCommandName(cmd)
	isStatus := name == "@status" || name == "@statusrequest" || name == "@statusextra"
	isClose := name == "@close" || name == "@closefiscalreceipt" ||
		name == "@closecashreceipt" || name == "@closenonfiscaldoc" ||
		name == "@refundclose"

	hadStatus := false
	if p.brand == BrandEpson {
		if m := ixEnvelopeRe.FindStringSubmatch(text); m != nil {
			hadStatus = true
			st.StatusByte = hexByte(m[1])
			st.ErrorByte = hexByte(m[2])
			text = strings.TrimPrefix(text, m[0])
		}
	}

	body, kind, code := splitIxResponse(text)

	if isStatus {
		if m := ixStatusRe.FindStringSubmatch(body); m != nil {
			hadStatus = true
			st.StatusByte = hexByte(m[1])
		}
		if m := ixErrorRe.FindStringSubmatch(body); m != nil {
			hadStatus = true
			st.ErrorByte = hexByte(m[1])
		}
	}
	applyIxFlags(&st)
	if isStatus {
		if m := ixDocStateRe.FindStringSubmatch(body); m != nil {
			st.DocumentOpen = strings.EqualFold(normalizeWord(m[1]), "abierto")
		}
	}
	st.StatusText = statusText(st.StatusByte)
	st.ErrorText = errorText(st.ErrorByte)

	if kind == "error" {
		eb := ixErrorCode(code)
		if eb == 0 {
			eb = st.ErrorByte
		}
		st.ErrorByte = eb
		// El código de error también determina los flags del guardián (fin de
		// papel, memoria llena): se recalcula con el código definitivo.
		applyIxFlags(&st)
		st.ErrorText = errorText(eb)
		st.LastError = strings.TrimSpace(body)
		if st.LastError == "" {
			st.LastError = st.ErrorText
		}
		d.Failed = true
		d.ErrorCode = eb
		d.ErrorText = st.LastError
		d.Status = st
		p.mu.Lock()
		p.last = st
		p.mu.Unlock()
		return d
	}

	if isClose {
		if m := ixDocRe.FindStringSubmatch(body); m != nil {
			num, _ := strconv.ParseInt(m[2], 10, 64)
			total := ""
			if t := ixTotalRe.FindStringSubmatch(body); t != nil {
				total = strings.TrimSpace(t[1])
			}
			d.Document = &Document{Type: ixDocType(m[1]), Number: num, Total: total}
		} else if strings.HasPrefix(strings.ToUpper(body), "DNF CERRADO") {
			// El documento no fiscal no reporta número en el protocolo IxBatch.
			d.Document = &Document{Type: DocNonFisc}
		}
	}

	if !hadStatus {
		// La impresora no informó estado en esta respuesta: no se arrastran
		// flags viejos (el siguiente @Status los vuelve a fijar).
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

func (p *IxBatch) cached() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *IxBatch) setOffline(err error) {
	p.mu.Lock()
	p.last.Online = false
	p.last.LastError = err.Error()
	p.last.Checked = p.now()
	p.mu.Unlock()
}

// ixCommandName extrae el nombre de comando de "@Comando|a|b": minúsculas y
// sin parámetros.
func ixCommandName(cmd []byte) string {
	raw := strings.TrimSpace(strings.TrimRight(string(cmd), "\r\n"))
	if !strings.HasPrefix(raw, "@") {
		return ""
	}
	if i := strings.IndexByte(raw, '|'); i >= 0 {
		raw = raw[:i]
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

// splitIxResponse separa el sobre "@Response|KIND|código|cuerpo". Devuelve
// kind vacío cuando el texto no trae sobre (por ejemplo el cuerpo de Epson ya
// despojado).
func splitIxResponse(text string) (body, kind, code string) {
	m := ixResponseRe.FindStringSubmatch(text)
	if m == nil {
		return text, "", ""
	}
	return strings.TrimPrefix(text, m[0]), strings.ToLower(m[1]), strings.TrimSpace(m[2])
}

// ixErrorCode acepta "0x50" (hexadecimal explícito) o "2" (decimal), que es
// como el protocolo de referencia reporta los errores.
func ixErrorCode(s string) byte {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "0x") {
		v, err := strconv.ParseUint(lower[2:], 16, 8)
		if err != nil {
			return 0
		}
		return byte(v)
	}
	v, err := strconv.ParseUint(s, 10, 8)
	if err != nil {
		return 0
	}
	return byte(v)
}

// applyIxFlags traduce los bytes de estado/error a los flags del guardián.
func applyIxFlags(st *Status) {
	st.PaperOut = st.ErrorByte == 0x01 || st.ErrorByte == 0x02 || st.ErrorByte == 0x03
	st.MemoryFull = st.ErrorByte == 0x6C ||
		st.StatusByte == 0x0A || st.StatusByte == 0x0B || st.StatusByte == 0x0C
	st.MemoryAlmostFull = st.StatusByte == 0x07 || st.StatusByte == 0x08 || st.StatusByte == 0x09
	switch st.StatusByte {
	case 0x02, 0x03, 0x05, 0x06, 0x08, 0x09, 0x0B, 0x0C:
		st.DocumentOpen = true
	case 0x01, 0x04, 0x07, 0x0A:
		st.DocumentOpen = false
	}
}

func ixDocType(label string) string {
	switch normalizeWord(label) {
	case "factura":
		return DocInvoice
	case "nota de credito":
		return DocCredit
	case "nota de debito":
		return DocDebit
	case "documento":
		return DocNonFisc
	}
	return DocUnknown
}

// normalizeWord baja a minúsculas y quita tildes de las etiquetas del
// protocolo, que pueden llegar en cualquiera de las dos formas.
func normalizeWord(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	replacer := strings.NewReplacer(
		"á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u",
		"Á", "a", "É", "e", "Í", "i", "Ó", "o", "Ú", "u",
	)
	return strings.Join(strings.Fields(replacer.Replace(s)), " ")
}
