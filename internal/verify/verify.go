// Package verify ejecuta la puesta en marcha de una impresora fiscal contra el
// hardware real: sondas de sólo lectura, estado, memoria fiscal y pruebas
// impresas opcionales. El informe resultante es evidencia y sirve de
// comparación tras cada actualización o cambio de equipo.
package verify

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/fiscal"
	"spoolerfiscal/internal/frame"
)

// Options controla qué pruebas impresas se ejecutan. Imprimir consume papel y
// (en el caso de un documento fiscal) correlativo: por eso son opt-in.
type Options struct {
	// PrintX imprime un reporte X (lectura: no reinicia acumulados).
	PrintX bool
	// PrintNonFiscal imprime un documento no fiscal de prueba.
	PrintNonFiscal bool
	// Timeout por sonda. 0 = 20 segundos.
	Timeout time.Duration
}

// Check es el resultado de una sonda o impresión.
type Check struct {
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
	OK      bool   `json:"ok"`
	Raw     string `json:"raw,omitempty"`
	Hex     string `json:"hex,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Info es la identificación de la impresora.
type Info struct {
	Brand         string  `json:"brand,omitempty"`
	Model         string  `json:"model,omitempty"`
	Serial        string  `json:"serial,omitempty"`
	RIF           string  `json:"rif,omitempty"`
	Firmware      string  `json:"firmware,omitempty"`
	Port          string  `json:"port,omitempty"`
	MemoryTotalMB float64 `json:"memory_total_mb,omitempty"`
	MemoryFreeMB  float64 `json:"memory_free_mb,omitempty"`
	LastInvoice   int64   `json:"last_invoice,omitempty"`
	LastCredit    int64   `json:"last_credit,omitempty"`
	LastDebit     int64   `json:"last_debit,omitempty"`
}

// Status es el estado reportado por la impresora.
type Status struct {
	Online       bool   `json:"online"`
	StatusByte   string `json:"status_byte,omitempty"`
	ErrorByte    string `json:"error_byte,omitempty"`
	StatusText   string `json:"status_text,omitempty"`
	ErrorText    string `json:"error_text,omitempty"`
	PaperOut     bool   `json:"paper_out"`
	MemoryFull   bool   `json:"memory_full"`
	MemoryNearly bool   `json:"memory_almost_full"`
	DocumentOpen bool   `json:"document_open"`
	Raw          string `json:"raw,omitempty"`
}

// Report es el resultado completo de la verificación.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Driver      string    `json:"driver"`
	Transport   string    `json:"transport"`
	Checks      []Check   `json:"checks"`
	Info        *Info     `json:"info,omitempty"`
	Status      *Status   `json:"status,omitempty"`
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
}

// Run conecta con la impresora configurada y ejecuta la verificación.
func Run(ctx context.Context, cfg config.PrinterCfg, log *slog.Logger, opts Options) (Report, error) {
	if log == nil {
		log = slog.Default()
	}
	rep := Report{GeneratedAt: time.Now(), Driver: strings.ToLower(strings.TrimSpace(cfg.Driver))}
	if rep.Driver == "" {
		rep.Driver = fiscal.BrandTFHKA
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}

	printer, err := buildPrinter(cfg, log)
	if err != nil {
		rep.Error = err.Error()
		return rep, err
	}
	rep.Transport = transportLabel(cfg)
	if err := printer.Open(ctx); err != nil {
		rep.Error = "no se pudo abrir la impresora: " + err.Error()
		return rep, errors.New(rep.Error)
	}
	defer func() { _ = printer.Close() }()

	// Sondas de sólo lectura del protocolo del driver.
	for _, cmd := range probeCommands(rep.Driver) {
		rep.Checks = append(rep.Checks, runProbe(ctx, printer, cfg, cmd, opts.Timeout))
	}

	// Estado interpretado por el driver (refresca el guardián y el documento).
	st, stErr := printer.GetStatus(ctx)
	rep.Status = statusOf(st, stErr)
	if stErr != nil {
		rep.Checks = append(rep.Checks, Check{Name: "estado_interpretado", OK: false, Error: stErr.Error()})
	} else {
		msg := boolError(st)
		rep.Checks = append(rep.Checks, Check{Name: "estado_interpretado", OK: msg == "", Raw: st.Raw, Error: msg})
	}

	// Identificación y memoria fiscal.
	info, infoErr := printer.GetInfo(ctx)
	rep.Info = infoOf(info)
	if infoErr != nil {
		rep.Checks = append(rep.Checks, Check{Name: "identificacion", OK: false, Error: infoErr.Error()})
	} else {
		rep.Checks = append(rep.Checks, Check{Name: "identificacion", OK: true})
	}

	if opts.PrintX {
		rep.Checks = append(rep.Checks, runPrint(ctx, printer, cfg, "reporte_x", []string{"U0X"}, opts.Timeout))
	}
	if opts.PrintNonFiscal {
		rep.Checks = append(rep.Checks, runPrint(ctx, printer, cfg, "documento_no_fiscal",
			[]string{"80$", "80!VERIFICACION SPOOLER FISCAL", "80*Prueba de impresion", "81"}, opts.Timeout))
	}

	rep.OK = true
	for _, c := range rep.Checks {
		if !c.OK {
			rep.OK = false
			break
		}
	}
	if rep.Status == nil || !rep.Status.Online || rep.Status.PaperOut || rep.Status.MemoryFull {
		rep.OK = false
	}
	return rep, nil
}

// buildPrinter construye el driver con el transporte configurado (serie o TCP).
func buildPrinter(cfg config.PrinterCfg, log *slog.Logger) (fiscal.Printer, error) {
	params, err := config.ParsePort(cfg.Port)
	if err != nil {
		return nil, err
	}
	var factory func() (frame.Stream, error)
	switch strings.ToLower(strings.TrimSpace(cfg.Transport)) {
	case "tcp":
		if strings.TrimSpace(cfg.TCPAddress) == "" {
			return nil, errors.New("printer.tcp_address es obligatorio con transport=tcp")
		}
		addr := cfg.TCPAddress
		dial := time.Duration(cfg.TCPDialTimeoutMS) * time.Millisecond
		if dial <= 0 {
			dial = 3 * time.Second
		}
		factory = func() (frame.Stream, error) { return fiscal.OpenTCP(addr, dial) }
	default:
		serialCfg := fiscal.SerialConfig{
			Name: cfg.Port.Name, BaudRate: params.BaudRate, DataBits: params.DataBits,
			Parity: params.Parity, StopBits: params.StopBits, FlowControl: params.FlowControl,
		}
		factory = func() (frame.Stream, error) { return fiscal.OpenSerial(serialCfg) }
	}
	return fiscal.New(cfg.Driver, fiscal.Options{
		NewTransport:     factory,
		ReadGap:          ms(cfg.ReadGapMS),
		ResponseTimeout:  ms(cfg.ResponseTimeoutMS),
		MaxResponseBytes: cfg.MaxResponseBytes,
		WriteSettle:      ms(cfg.WriteSettleMS),
		StatusProbe:      cfg.StatusProbe,
		CommandEnding:    fiscal.NormalizeCommandTerminator(cfg.Driver, cfg.CommandTerminator),
		Logger:           log,
	})
}

func probeCommands(driver string) []string {
	switch driver {
	case fiscal.BrandHasar, fiscal.BrandBixolon, fiscal.BrandEpson:
		return []string{"@Status"}
	default:
		return []string{"S1", "S2", "S5"}
	}
}

// runProbe envía un comando de sólo lectura y registra la respuesta cruda.
func runProbe(ctx context.Context, p fiscal.Printer, cfg config.PrinterCfg, cmd string, timeout time.Duration) Check {
	c := Check{Name: probeName(cmd), Command: cmd}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ending := fiscal.NormalizeCommandTerminator(cfg.Driver, cfg.CommandTerminator)
	if err := p.SendCommand(callCtx, []byte(cmd+ending)); err != nil {
		c.Error = err.Error()
		return c
	}
	resp, err := p.ReadResponse(callCtx)
	if err != nil {
		c.Error = err.Error()
		return c
	}
	c.OK = true
	c.Raw = strings.TrimSpace(string(resp))
	c.Hex = hex.EncodeToString(resp)
	if d := p.Describe([]byte(cmd), resp); d.Failed {
		c.OK = false
		c.Error = d.ErrorText
	}
	return c
}

// runPrint ejecuta una secuencia impresa y exige que todas las respuestas sean
// correctas.
func runPrint(ctx context.Context, p fiscal.Printer, cfg config.PrinterCfg, name string, commands []string, timeout time.Duration) Check {
	c := Check{Name: name, Command: strings.Join(commands, " / ")}
	var raws []string
	ending := fiscal.NormalizeCommandTerminator(cfg.Driver, cfg.CommandTerminator)
	for _, cmd := range commands {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		err := p.SendCommand(callCtx, []byte(cmd+ending))
		var resp []byte
		if err == nil {
			resp, err = p.ReadResponse(callCtx)
		}
		cancel()
		if err != nil {
			c.Error = fmt.Sprintf("%s: %v", cmd, err)
			return c
		}
		if d := p.Describe([]byte(cmd), resp); d.Failed {
			c.Error = fmt.Sprintf("%s: %s", cmd, d.ErrorText)
			return c
		}
		raws = append(raws, strings.TrimSpace(string(resp)))
	}
	c.OK = true
	c.Raw = strings.Join(raws, " | ")
	return c
}

func probeName(cmd string) string {
	switch cmd {
	case "S1":
		return "identificacion_s1"
	case "S2":
		return "estado_s2"
	case "S5":
		return "memoria_fiscal_s5"
	case "@Status":
		return "estado"
	}
	return strings.ToLower(cmd)
}

func statusOf(st fiscal.Status, err error) *Status {
	out := &Status{
		Online: st.Online, StatusByte: fmt.Sprintf("0x%02X", st.StatusByte),
		ErrorByte: fmt.Sprintf("0x%02X", st.ErrorByte), StatusText: st.StatusText,
		ErrorText: st.ErrorText, PaperOut: st.PaperOut, MemoryFull: st.MemoryFull,
		MemoryNearly: st.MemoryAlmostFull, DocumentOpen: st.DocumentOpen, Raw: st.Raw,
	}
	if err != nil {
		out.Online = false
	}
	return out
}

func infoOf(i fiscal.Info) *Info {
	return &Info{
		Brand: i.Brand, Model: i.Model, Serial: i.Serial, RIF: i.RIF,
		Firmware: i.Firmware, Port: i.Port,
		MemoryTotalMB: i.MemoryTotalMB, MemoryFreeMB: i.MemoryFreeMB,
		LastInvoice: i.LastInvoice, LastCredit: i.LastCredit, LastDebit: i.LastDebit,
	}
}

// boolError convierte los flags de estado en un mensaje de error legible.
func boolError(st fiscal.Status) string {
	switch {
	case st.PaperOut:
		return "fin de papel"
	case st.MemoryFull:
		return "memoria fiscal llena"
	case st.LastError != "":
		return st.LastError
	}
	return ""
}

func transportLabel(cfg config.PrinterCfg) string {
	if strings.EqualFold(strings.TrimSpace(cfg.Transport), "tcp") {
		return "tcp " + cfg.TCPAddress
	}
	return fmt.Sprintf("serial %s @ %d 8N1", cfg.Port.Name, cfg.Port.BaudRate)
}

// Compare contrasta el informe actual con la línea base: el equipo debe ser el
// mismo (serial y RIF) y el correlativo no puede retroceder. Devuelve la lista
// de problemas; vacía = igual.
func Compare(base, current Report) []string {
	var problems []string
	if base.Driver != "" && current.Driver != "" && base.Driver != current.Driver {
		problems = append(problems, fmt.Sprintf("cambió el driver: base %q, actual %q", base.Driver, current.Driver))
	}
	if base.Info == nil || current.Info == nil {
		return problems
	}
	if base.Info.Serial != "" && current.Info.Serial != "" && base.Info.Serial != current.Info.Serial {
		problems = append(problems, fmt.Sprintf("cambió el serial de la impresora: base %q, actual %q",
			base.Info.Serial, current.Info.Serial))
	}
	if base.Info.RIF != "" && current.Info.RIF != "" && !strings.EqualFold(base.Info.RIF, current.Info.RIF) {
		problems = append(problems, fmt.Sprintf("cambió el RIF fiscal: base %q, actual %q",
			base.Info.RIF, current.Info.RIF))
	}
	if base.Info.LastInvoice > 0 && current.Info.LastInvoice > 0 &&
		current.Info.LastInvoice < base.Info.LastInvoice {
		problems = append(problems, fmt.Sprintf("el correlativo retrocedió: base %d, actual %d",
			base.Info.LastInvoice, current.Info.LastInvoice))
	}
	return problems
}

func ms(v int) time.Duration {
	if v <= 0 {
		return 0
	}
	return time.Duration(v) * time.Millisecond
}
