// simprint emula una impresora fiscal TFHKA a nivel de protocolo, pero
// imprime con papel real en una térmica común de 80 mm (ESC/POS). Sirve para
// probar el spooler completo —correlativo, cola, pausa por papel— sin una
// impresora fiscal certificada.
//
// Lo que imprime NO es un documento fiscal válido.
//
// Ejemplos:
//
//	simprint -listen 127.0.0.1:2011 -escpos-tcp 192.168.1.50:9100
//	simprint -listen 127.0.0.1:2011 -com COM3
//	simprint -listen 127.0.0.1:2011                (solo texto en consola)
//
// Con el servidor apuntado a printer.transport = tcp y
// printer.tcp_address = 127.0.0.1:2011, toda factura del spooler sale por la
// térmica. La tecla "p" + Enter alterna el sensor de papel (para probar la
// pausa y reanudación de la cola).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"spoolerfiscal/internal/escpos"
	"spoolerfiscal/internal/fiscal"
	"spoolerfiscal/internal/frame"
	"spoolerfiscal/internal/simprint"
	"spoolerfiscal/internal/winprint"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

const usage = `Uso: simprint [opciones]

Emulador de impresora fiscal TFHKA con salida a térmica ESC/POS. Escucha el
protocolo ASCII del spooler (o de Profit Plus) y al cerrar cada documento lo
imprime en papel real. No emite documentos fiscales válidos.

Opciones:
  -listen <ip:puerto>   Escucha el protocolo TFHKA por TCP (vacío = desactivado).
  -serial-protocol COM  Escucha el protocolo TFHKA por un puerto serie
                        (útil con com0com para emular el puerto de la fiscal).
  -escpos-tcp <ip:9100> Térmica de red: envía el ticket al puerto 9100.
  -com COMx             Térmica por USB/serie: envía el ticket a ese puerto COM.
  -win-printer <nombre>  Térmica instalada en Windows (USB00x): envía el ticket
                        en RAW por el spooler (p. ej. "Xprinter XP-80").
  -baud N               Velocidad de la térmica serie (por omisión 9600).
  -text-file <archivo>  Además de imprimir, agrega el ticket en texto plano.
  -state <archivo>      Estado del emulador (correlativos). Por omisión
                        data\simprint_state.json.
  -rif J-...            RIF del encabezado (por omisión J-12345678-9).
  -serial-id SIM...     Serial del emulador que responde en S1/S5.
  -store "Nombre"       Nombre del punto de venta en el encabezado.

Controles:
  p + Enter   alterna el sensor de papel (0x01 en S2 y ERROR al imprimir).
  Ctrl+C      cierra y guarda el estado.

Ejemplo con el servidor:
  simprint -listen 127.0.0.1:2011 -escpos-tcp 192.168.1.50:9100
  (server.yaml: printer.transport = tcp, printer.tcp_address = 127.0.0.1:2011)
`

func run(args []string) int {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fs := flag.NewFlagSet("simprint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	listen := fs.String("listen", "127.0.0.1:2011", "escucha del protocolo TFHKA por TCP (vacío = desactivado)")
	serialProto := fs.String("serial-protocol", "", "escucha del protocolo por puerto COM (opcional)")
	escposTCP := fs.String("escpos-tcp", "", "térmica de red ip:9100")
	com := fs.String("com", "", "térmica por puerto COM")
	winPrinter := fs.String("win-printer", "", `térmica instalada en Windows (USB00x), p. ej. "Xprinter XP-80"`)
	baud := fs.Int("baud", 9600, "velocidad de la térmica serie")
	textFile := fs.String("text-file", "", "archivo de texto plano de respaldo")
	statePath := fs.String("state", filepath.Join("data", "simprint_state.json"), "estado del emulador")
	rif := fs.String("rif", "J-12345678-9", "RIF del encabezado")
	serialID := fs.String("serial-id", "SIMTFHKA01", "serial del emulador")
	store := fs.String("store", "PUNTO DE VENTA SIMULADO", "nombre del punto de venta")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *listen == "" && *serialProto == "" {
		log.Error("nada que escuchar: indique -listen o -serial-protocol")
		return 2
	}

	cfg := simprint.Config{SerialID: *serialID, RIF: *rif, Store: *store}
	engine, err := simprint.NewEngine(cfg, *statePath)
	if err != nil {
		log.Error("estado del emulador ilegible", "error", err)
		return 1
	}

	// Salida de impresión: térmica (ESC/POS) y/o texto, más la vista previa en
	// consola. El motor entrega los documentos cerrados por aquí.
	printer, err := newPrinter(cfg, *escposTCP, *com, *winPrinter, *baud, *textFile, log)
	if err != nil {
		log.Error("térmica no disponible", "error", err)
		return 1
	}
	engine.OnDocument = func(doc simprint.Document) {
		text := simprint.RenderText(doc, cfg)
		fmt.Println("\n" + text) // vista previa en consola
		if err := printer.print(doc, text); err != nil {
			log.Warn("salida de impresión falló", "error", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Control del sensor de papel: "p" + Enter alterna.
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 256), 256)
		for sc.Scan() {
			switch strings.TrimSpace(sc.Text()) {
			case "p":
				on := engine.SetPaperOut(!engine.PaperOut())
				if on {
					log.Warn("papel agotado (simulado): S2 reporta 0x01 y los comandos de impresión devuelven ERROR|0x01")
				} else {
					log.Info("papel repuesto (simulado)")
				}
			case "estado", "s":
				st := engine.StateSnapshot()
				log.Info("estado del emulador", "facturas", st.Invoices, "nc", st.Credit, "nd", st.Debit, "no_fiscal", st.NonFiscal, "reportes", st.Reports, "total", st.Total, "iva", st.Tax)
			case "salir", "q":
				stop()
				return
			}
		}
	}()

	log.Info("simprint iniciado (documento de PRUEBA, no fiscal)",
		"protocolo_tcp", *listen, "protocolo_com", *serialProto,
		"termica", printer.describe(), "estado", *statePath)

	var wg sync.WaitGroup
	if *listen != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serveTCP(ctx, *listen, engine, log); err != nil {
				log.Error("escucha de protocolo", "error", err)
			}
		}()
	}
	if *serialProto != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serveSerial(ctx, *serialProto, engine, log); err != nil {
				log.Error("escucha serie de protocolo", "error", err)
			}
		}()
	}

	<-ctx.Done()
	stop()
	log.Info("cerrando simprint")
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown // las goroutines cierran sus listeners con el contexto
	if err := engine.Save(); err != nil {
		log.Warn("estado no guardado", "error", err)
	}
	printer.close()
	wg.Wait()
	log.Info("simprint detenido", "facturas", engine.StateSnapshot().Invoices)
	return 0
}

// serveTCP atiende el protocolo TFHKA por TCP: un comando por trama (gap de
// silencio), una respuesta por comando. Múltiples conexiones simultáneas.
func serveTCP(ctx context.Context, addr string, engine *simprint.Engine, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("escuchar %s: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("aceptar: %w", err)
		}
		go handleStream(ctx, fiscal.WrapTCP(conn), engine, log, conn.RemoteAddr().String())
	}
}

// handleStream procesa comandos de un stream (TCP o COM) hasta que cierra.
func handleStream(ctx context.Context, s frame.Stream, engine *simprint.Engine, log *slog.Logger, peer string) {
	defer s.Close()
	log.Debug("conexión de protocolo", "peer", peer)
	for {
		if ctx.Err() != nil {
			return
		}
		raw, err := frame.Read(ctx, s, 20*time.Millisecond, 5*time.Second, 8192)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			return // timeout de silencio entre comandos o cliente cerrado
		}
		resp := engine.Handle(string(raw))
		if err := frame.Write(s, []byte(resp+"\n")); err != nil {
			return
		}
	}
}

// serveSerial escucha el protocolo por un COM real (con com0com, el par
// virtual). Un solo cliente: el driver de la fiscal.
func serveSerial(ctx context.Context, name string, engine *simprint.Engine, log *slog.Logger) error {
	port, err := fiscal.OpenSerial(fiscal.SerialConfig{Name: name, BaudRate: 9600, DataBits: 8, Parity: "N", StopBits: "1"})
	if err != nil {
		return fmt.Errorf("abrir %s: %w", name, err)
	}
	defer port.Close()
	go func() {
		<-ctx.Done()
		_ = port.Close()
	}()
	handleStream(ctx, port, engine, log, name)
	return nil
}

// printer agrupa las salidas configuradas de los documentos cerrados.
type printer struct {
	mu       sync.Mutex
	cfg      simprint.Config // encabezados del ticket (RIF, serial, punto de venta)
	tcpAddr  string          // térmica de red
	serial   frame.Stream
	serialLR string
	winName  string // cola de impresión de Windows (USB00x)
	textPath string
	log      *slog.Logger
}

func newPrinter(cfg simprint.Config, tcpAddr, com, winName string, baud int, textPath string, log *slog.Logger) (*printer, error) {
	p := &printer{cfg: cfg, tcpAddr: tcpAddr, winName: winName, textPath: textPath, log: log}
	if com != "" {
		s, err := fiscal.OpenSerial(fiscal.SerialConfig{Name: com, BaudRate: baud, DataBits: 8, Parity: "N", StopBits: "1"})
		if err != nil {
			return nil, fmt.Errorf("abrir térmica %s: %w", com, err)
		}
		p.serial = s
		p.serialLR = fmt.Sprintf("COM %s @ %d", com, baud)
	}
	return p, nil
}

func (p *printer) describe() string {
	switch {
	case p.winName != "":
		return "windows " + p.winName
	case p.tcpAddr != "":
		return "tcp " + p.tcpAddr
	case p.serial != nil:
		return p.serialLR
	default:
		return "consola (texto)"
	}
}

// print envía el documento: bytes ESC/POS a la térmica y texto plano al
// archivo de respaldo.
func (p *printer) print(doc simprint.Document, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.winName != "" {
		if err := winprint.Print(p.winName, escpos.Ticket(doc, p.cfg)); err != nil {
			p.log.Warn("spooler de Windows no imprimió", "impresora", p.winName, "error", err)
		}
	}
	if p.tcpAddr != "" {
		if err := p.sendTCP(doc); err != nil {
			p.log.Warn("térmica de red no respondió", "addr", p.tcpAddr, "error", err)
		}
	}
	if p.serial != nil {
		if err := frame.Write(p.serial, escpos.Ticket(doc, p.cfg)); err != nil {
			p.log.Warn("térmica serie no respondió", "error", err)
		}
	}
	if p.textPath != "" {
		f, err := os.OpenFile(p.textPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.WriteString(text + "\n"); err != nil {
			return err
		}
	}
	return nil
}

// sendTCP conecta, imprime y desconecta por cada documento (como hace un
// driver de Windows con el puerto 9100).
func (p *printer) sendTCP(doc simprint.Document) error {
	conn, err := net.DialTimeout("tcp", p.tcpAddr, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.Write(escpos.Ticket(doc, p.cfg))
	return err
}

func (p *printer) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.serial != nil {
		_ = p.serial.Close()
		p.serial = nil
	}
}
