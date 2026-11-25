//go:build windows

// Command detectar busca impresoras fiscales conectadas a este equipo. Enumera
// los puertos serie de Windows, las colas de impresiÃ³n instaladas y, si se
// pide, direcciones TCP; luego envÃ­a sondas de sÃ³lo lectura del protocolo
// TFHKA (S2 estado y S1 identificaciÃ³n) a cada candidato.
//
//	.\detectar.exe
//	.\detectar.exe -ports COM1,COM3 -bauds 9600,115200
//	.\detectar.exe -tcp 192.168.1.50:9100
//	.\detectar.exe -red 192.168.0.0/24
//	.\detectar.exe -json > deteccion.json
//
// No imprime ni consume correlativo. Sale con cÃ³digo 0 si encuentra al menos
// una impresora fiscal y con 1 si no encuentra ninguna.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial/enumerator"

	"spoolerfiscal/internal/fiscal"
	"spoolerfiscal/internal/frame"
	"spoolerfiscal/internal/winprint"
)

// portInfo es un puerto serie tal como lo enumera Windows.
type portInfo struct {
	Name         string `json:"name"`
	Product      string `json:"product,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
	VID          string `json:"vid,omitempty"`
	PID          string `json:"pid,omitempty"`
	IsUSB        bool   `json:"is_usb,omitempty"`
}

// probeResult es el resultado de sondear un candidato.
type probeResult struct {
	Target        string  `json:"target"`
	Baud          int     `json:"baud,omitempty"`
	Responded     bool    `json:"responded"`
	Fiscal        bool    `json:"fiscal"`
	Serial        string  `json:"serial,omitempty"`
	RIF           string  `json:"rif,omitempty"`
	Model         string  `json:"model,omitempty"`
	LastInvoice   int64   `json:"last_invoice,omitempty"`
	MemoryTotalMB float64 `json:"memory_total_mb,omitempty"`
	MemoryFreeMB  float64 `json:"memory_free_mb,omitempty"`
	Status        string  `json:"status,omitempty"`
	Raw           string  `json:"raw,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// report es la salida completa, tambiÃ©n en JSON.
type report struct {
	GeneratedAt     time.Time     `json:"generated_at"`
	SerialPorts     []portInfo    `json:"serial_ports"`
	WindowsPrinters []string      `json:"windows_printers,omitempty"`
	NetworkOpen     []string      `json:"network_open,omitempty"`
	Results         []probeResult `json:"results"`
	Detected        int           `json:"detected"`
}

func main() {
	portsFlag := flag.String("ports", "", "puertos a sondear (coma); vacÃ­o = todos los detectados")
	baudsFlag := flag.String("bauds", "9600,115200", "velocidades a probar (coma)")
	tcpFlag := flag.String("tcp", "", "direcciones TCP a sondear, ej. 192.168.1.50:9100")
	redFlag := flag.String("red", "", "rango CIDR a escanear por TCP, ej. 192.168.0.0/24 (mÃ¡x /22)")
	tcpPortsFlag := flag.String("tcp-ports", "9100,2011", "puertos TCP del escaneo de red")
	timeout := flag.Duration("timeout", 1500*time.Millisecond, "espera por sonda")
	listOnly := flag.Bool("list", false, "sÃ³lo enumerar puertos e impresoras, sin sondear")
	noPrinters := flag.Bool("no-printers", false, "no enumerar colas de impresiÃ³n de Windows")
	jsonOut := flag.Bool("json", false, "emitir el informe en JSON")
	flag.Parse()

	rep := report{GeneratedAt: time.Now()}

	// Puertos serie.
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		fmt.Fprintf(os.Stderr, "detectar: enumerar puertos serie: %v\n", err)
	} else {
		for _, d := range details {
			rep.SerialPorts = append(rep.SerialPorts, portInfo{
				Name: d.Name, Product: d.Product, SerialNumber: d.SerialNumber,
				VID: d.VID, PID: d.PID, IsUSB: d.IsUSB,
			})
		}
	}
	sort.Slice(rep.SerialPorts, func(i, j int) bool { return rep.SerialPorts[i].Name < rep.SerialPorts[j].Name })

	// Impresoras instaladas en Windows (colas locales y de red).
	if !*noPrinters {
		if names, perr := winprint.List(); perr != nil {
			fmt.Fprintf(os.Stderr, "detectar: enumerar impresoras de Windows: %v\n", perr)
		} else {
			sort.Strings(names)
			rep.WindowsPrinters = names
		}
	}

	if !*jsonOut {
		printPorts(rep)
	}

	if *listOnly {
		emit(rep, *jsonOut)
		if len(rep.SerialPorts) == 0 {
			printNoPortsHint()
			os.Exit(1)
		}
		return
	}

	// Candidatos serie.
	targets := rep.SerialPorts
	if strings.TrimSpace(*portsFlag) != "" {
		targets = nil
		for _, name := range splitList(*portsFlag) {
			targets = append(targets, portInfo{Name: name})
		}
	}
	bauds := parseInts(*baudsFlag)
	if len(bauds) == 0 {
		bauds = []int{9600}
	}

	if len(targets) == 0 {
		fmt.Println("No hay puertos serie que sondear.")
	} else if !*jsonOut {
		fmt.Println("\nSondeando impresoras fiscales TFHKA (no imprime nada):")
	}
	for _, p := range targets {
		for _, baud := range bauds {
			name, b := p.Name, baud
			res := probeTFHKA(name, b, *timeout, func() (frame.Stream, error) {
				return fiscal.OpenSerial(fiscal.SerialConfig{
					Name: name, BaudRate: baud, DataBits: 8,
					Parity: "none", StopBits: "1", FlowControl: "none",
				})
			})
			rep.Results = append(rep.Results, res)
			if !*jsonOut {
				printResult(res)
			}
			if res.Fiscal {
				break // ya identificada en esta velocidad
			}
		}
	}

	// Candidatos TCP explÃ­citos.
	for _, addr := range splitList(*tcpFlag) {
		a := addr
		res := probeTFHKA(a, 0, *timeout, func() (frame.Stream, error) {
			return fiscal.OpenTCP(a, 2*time.Second)
		})
		rep.Results = append(rep.Results, res)
		if !*jsonOut {
			printResult(res)
		}
	}

	// Escaneo de un rango de red: sÃ³lo se sondean por protocolo los equipos que
	// aceptan conexiÃ³n en los puertos indicados (impresoras de red y puentes
	// serie/IP suelen escuchar en 9100 o 2011).
	if strings.TrimSpace(*redFlag) != "" {
		ports := parseInts(*tcpPortsFlag) // mismos enteros positivos
		if len(ports) == 0 {
			ports = []int{9100, 2011}
		}
		if !*jsonOut {
			fmt.Printf("\nEscaneando %s (puertos %s)...\n", *redFlag, *tcpPortsFlag)
		}
		open, err := scanNetwork(*redFlag, ports, 400*time.Millisecond)
		if err != nil {
			fmt.Fprintf(os.Stderr, "detectar: escaneo de red: %v\n", err)
		} else {
			rep.NetworkOpen = open
			if !*jsonOut {
				if len(open) == 0 {
					fmt.Println("  NingÃºn equipo aceptÃ³ conexiÃ³n en esos puertos.")
				} else {
					fmt.Printf("  Equipos con puerto abierto: %d\n", len(open))
				}
			}
			for _, addr := range open {
				a := addr
				res := probeTFHKA(a, 0, *timeout, func() (frame.Stream, error) {
					return fiscal.OpenTCP(a, 2*time.Second)
				})
				rep.Results = append(rep.Results, res)
				if !*jsonOut {
					printResult(res)
				}
			}
		}
	}

	for _, r := range rep.Results {
		if r.Fiscal {
			rep.Detected++
		}
	}
	emit(rep, *jsonOut)

	if rep.Detected == 0 {
		fmt.Println("\nNo se detectÃ³ ninguna impresora fiscal.")
		if len(rep.SerialPorts) == 0 {
			printNoPortsHint()
		} else {
			fmt.Println("Revise que la impresora estÃ© encendida, el cable conectado y la velocidad (baudios).")
			fmt.Println("Si el puerto estÃ¡ ocupado por el servicio, detÃ©ngalo: sc stop SpoolerFiscalServer")
		}
		os.Exit(1)
	}
	fmt.Printf("\nImpresoras fiscales detectadas: %d\n", rep.Detected)
}

// probeTFHKA abre un transporte, consulta estado (S2) e identificaciÃ³n (S1/S5).
func probeTFHKA(name string, baud int, timeout time.Duration, newTransport func() (frame.Stream, error)) probeResult {
	res := probeResult{Target: name, Baud: baud}
	ctx, cancel := context.WithTimeout(context.Background(), timeout*4)
	defer cancel()

	printer, err := fiscal.New("tfhka", fiscal.Options{
		NewTransport:     newTransport,
		ReadGap:          50 * time.Millisecond,
		ResponseTimeout:  timeout,
		MaxResponseBytes: 32 * 1024,
		StatusProbe:      "S2",
		CommandEnding:    "\r\n",
		PortLabel:        name,
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if err := printer.Open(ctx); err != nil {
		res.Error = err.Error()
		return res
	}
	defer func() { _ = printer.Close() }()

	st, stErr := printer.GetStatus(ctx)
	res.Raw = st.Raw
	if st.Raw != "" {
		res.Responded = true
	}
	res.Status = strings.TrimSpace(strings.Join(nonEmpty(st.StatusText, st.ErrorText), " / "))
	if res.Status != "" {
		res.Responded = true
	}
	if stErr != nil && !res.Responded {
		res.Error = stErr.Error()
		return res
	}

	info, infoErr := printer.GetInfo(ctx)
	if info.Serial != "" || info.RIF != "" {
		res.Responded = true
		res.Fiscal = true
		res.Serial = info.Serial
		res.RIF = info.RIF
		res.Model = strings.TrimSpace(info.Brand + " " + info.Model)
		res.LastInvoice = info.LastInvoice
		res.MemoryTotalMB = info.MemoryTotalMB
		res.MemoryFreeMB = info.MemoryFreeMB
	}
	if infoErr != nil && !res.Fiscal {
		res.Error = infoErr.Error()
	}
	return res
}

// scanNetwork devuelve las direcciones ip:puerto que aceptan conexiÃ³n TCP.
// Limita el rango a 1024 direcciones para que un CIDR equivocado no lance un
// escaneo enorme.
func scanNetwork(cidr string, ports []int, dialTimeout time.Duration) ([]string, error) {
	_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return nil, fmt.Errorf("rango %q invÃ¡lido: %w", cidr, err)
	}
	ones, bits := network.Mask.Size()
	total := 1 << (bits - ones)
	if total > 1024 {
		return nil, fmt.Errorf("%s tiene %d direcciones; use un rango de /22 o menor", cidr, total)
	}

	targets := make([]string, 0, total*len(ports))
	for ip := append(net.IP(nil), network.IP.Mask(network.Mask)...); network.Contains(ip); incIP(ip) {
		for _, port := range ports {
			targets = append(targets, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	open := make([]string, 0)
	for _, addr := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(addr string) {
			defer wg.Done()
			defer func() { <-sem }()
			conn, derr := net.DialTimeout("tcp", addr, dialTimeout)
			if derr != nil {
				return
			}
			_ = conn.Close()
			mu.Lock()
			open = append(open, addr)
			mu.Unlock()
		}(addr)
	}
	wg.Wait()
	sort.Strings(open)
	return open, nil
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] != 0 {
			break
		}
	}
}

func printPorts(rep report) {
	if len(rep.SerialPorts) == 0 {
		fmt.Println("Puertos serie detectados por Windows: ninguno")
	} else {
		fmt.Printf("Puertos serie detectados por Windows: %d\n", len(rep.SerialPorts))
		for _, p := range rep.SerialPorts {
			extra := []string{}
			if p.Product != "" {
				extra = append(extra, p.Product)
			}
			if p.IsUSB {
				usb := "USB"
				if p.VID != "" && p.PID != "" {
					usb += fmt.Sprintf(" %s:%s", p.VID, p.PID)
				}
				if p.SerialNumber != "" {
					usb += " s/n " + p.SerialNumber
				}
				extra = append(extra, usb)
			}
			if len(extra) == 0 {
				fmt.Printf("  - %s\n", p.Name)
			} else {
				fmt.Printf("  - %s  (%s)\n", p.Name, strings.Join(extra, ", "))
			}
		}
	}
	if len(rep.WindowsPrinters) == 0 {
		fmt.Println("Impresoras instaladas en Windows: ninguna")
	} else {
		fmt.Printf("Impresoras instaladas en Windows: %d\n", len(rep.WindowsPrinters))
		for _, name := range rep.WindowsPrinters {
			fmt.Printf("  - %s\n", name)
		}
	}
}

func printResult(r probeResult) {
	target := r.Target
	if r.Baud > 0 {
		target = fmt.Sprintf("%s @ %d 8N1", r.Target, r.Baud)
	}
	switch {
	case r.Fiscal:
		fmt.Printf("  [OK   ] %-22s impresora fiscal\n", target)
		fmt.Printf("           serial=%s RIF=%s modelo=%s\n", orDash(r.Serial), orDash(r.RIF), orDash(r.Model))
		if r.LastInvoice > 0 {
			fmt.Printf("           Ãºltima factura=%d memoria=%.0f/%.0f MB\n",
				r.LastInvoice, r.MemoryFreeMB, r.MemoryTotalMB)
		}
		if r.Status != "" {
			fmt.Printf("           estado=%s\n", r.Status)
		}
	case r.Responded:
		fmt.Printf("  [??????] %-22s respondiÃ³ pero sin identificaciÃ³n fiscal\n", target)
		fmt.Printf("           respuesta=%s\n", clip(r.Raw, 80))
	case r.Error != "":
		fmt.Printf("  [ -   ] %-22s %s\n", target, clip(r.Error, 80))
	default:
		fmt.Printf("  [ -   ] %-22s sin respuesta\n", target)
	}
}

func printNoPortsHint() {
	fmt.Println()
	fmt.Println("Windows no expone ningÃºn puerto serie. Una impresora conectada por COM")
	fmt.Println("no puede abrirse hasta que exista el puerto. Revise en el Administrador")
	fmt.Println("de dispositivos si hay un 'Puerto serie PCI' o un adaptador USB-serie con")
	fmt.Println("signo de admiraciÃ³n (controlador sin instalar) o si el cable estÃ¡ desconectado.")
}

// emit imprime el informe JSON cuando se pide, o nada si la salida humana ya se
// mostrÃ³. En modo -json la enumeraciÃ³n tambiÃ©n se imprime como JSON.
func emit(rep report, asJSON bool) {
	if !asJSON {
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}

func splitList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func parseInts(s string) []int {
	out := []int{}
	for _, part := range splitList(s) {
		if v, err := strconv.Atoi(part); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "â€”"
	}
	return s
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\r", " ")
	s = strings.ReplaceAll(s, "\n", " | ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
