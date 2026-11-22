// Command verificar ejecuta la puesta en marcha de la impresora fiscal:
// sondas de sólo lectura (identificación, estado, memoria fiscal) y,
// opcionalmente, un reporte X y un documento no fiscal de prueba. Escribe un
// informe JSON que sirve de evidencia y de línea base tras cada actualización.
//
//	.\verificar.exe -config ..\server\configs\server.yaml
//	.\verificar.exe -config ..\server\configs\server.yaml -x -out reporte.json
//	.\verificar.exe -config ..\server\configs\server.yaml -nonfiscal -yes
//
// Sale con código 1 si alguna comprobación falla, para poder encadenarlo en un
// script de instalación o de actualización.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/verify"
)

func main() {
	configPath := flag.String("config", "configs/server.yaml", "server.yaml con la sección printer")
	out := flag.String("out", "", "archivo JSON del informe (vacío = sólo consola)")
	comparePath := flag.String("compare", "", "informe base para comparar serial, RIF y correlativo")
	printX := flag.Bool("x", false, "imprimir reporte X (lectura, no reinicia acumulados)")
	printNF := flag.Bool("nonfiscal", false, "imprimir documento no fiscal de prueba")
	yes := flag.Bool("yes", false, "no pedir confirmación para las pruebas impresas")
	jsonOnly := flag.Bool("json", false, "emitir sólo el informe JSON")
	timeout := flag.Duration("timeout", 20*time.Second, "timeout por sonda")
	flag.Parse()

	if err := config.LoadExecutableDotEnv(); err != nil {
		fatalf("entorno: %v", err)
	}

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		fatalf("configuración: %v", err)
	}

	if (*printX || *printNF) && !*yes {
		if !confirm("Esto imprimirá en la impresora fiscal. ¿Continuar? [s/N] ") {
			fmt.Println("Cancelado.")
			os.Exit(1)
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()
	rep, err := verify.Run(ctx, cfg.Printer, logger, verify.Options{
		PrintX: *printX, PrintNonFiscal: *printNF, Timeout: *timeout,
	})

	encoded, jerr := json.MarshalIndent(rep, "", "  ")
	if jerr != nil {
		fatalf("serializar informe: %v", jerr)
	}
	if *out != "" {
		if werr := os.WriteFile(*out, append(encoded, '\n'), 0o644); werr != nil {
			fatalf("escribir informe: %v", werr)
		}
	}

	if *jsonOnly {
		fmt.Println(string(encoded))
	} else {
		printHuman(rep, *out)
	}

	problems, cerr := compareWith(*comparePath, rep)
	if cerr != nil {
		fatalf("comparar: %v", cerr)
	}
	if len(problems) > 0 {
		fmt.Println("\nComparación con la línea base: DIFERENCIAS")
		for _, p := range problems {
			fmt.Printf("  - %s\n", p)
		}
		os.Exit(1)
	}

	if err != nil || !rep.OK {
		os.Exit(1)
	}
}

// compareWith carga la línea base y la contrasta con el informe actual.
func compareWith(path string, current verify.Report) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var base verify.Report
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	problems := verify.Compare(base, current)
	if len(problems) == 0 {
		fmt.Println("\nComparación con la línea base: sin diferencias")
	}
	return problems, nil
}

func printHuman(rep verify.Report, out string) {
	fmt.Printf("Verificación de impresora fiscal\n")
	fmt.Printf("  driver:    %s\n", rep.Driver)
	fmt.Printf("  transporte: %s\n", rep.Transport)
	fmt.Printf("  fecha:     %s\n\n", rep.GeneratedAt.Format(time.RFC3339))

	for _, c := range rep.Checks {
		status := "OK   "
		if !c.OK {
			status = "FALLA"
		}
		detail := c.Raw
		if c.Error != "" {
			if detail != "" {
				detail += " — "
			}
			detail += c.Error
		}
		if len(detail) > 100 {
			detail = detail[:97] + "..."
		}
		fmt.Printf("  [%s] %-22s %s\n", status, c.Name, detail)
	}

	if rep.Info != nil {
		fmt.Printf("\n  Impresora\n")
		fmt.Printf("    marca/modelo: %s %s\n", rep.Info.Brand, rep.Info.Model)
		fmt.Printf("    serial:       %s\n", orDash(rep.Info.Serial))
		fmt.Printf("    RIF:          %s\n", orDash(rep.Info.RIF))
		if rep.Info.MemoryTotalMB > 0 {
			fmt.Printf("    memoria:      %.0f MB (libre %.0f MB)\n",
				rep.Info.MemoryTotalMB, rep.Info.MemoryFreeMB)
		}
		if rep.Info.LastInvoice > 0 {
			fmt.Printf("    última factura: %d (NC %d, ND %d)\n",
				rep.Info.LastInvoice, rep.Info.LastCredit, rep.Info.LastDebit)
		}
	}
	if rep.Status != nil {
		fmt.Printf("\n  Estado: online=%v papel_agotado=%v memoria_llena=%v documento_abierto=%v\n",
			rep.Status.Online, rep.Status.PaperOut, rep.Status.MemoryFull, rep.Status.DocumentOpen)
	}
	if out != "" {
		fmt.Printf("\n  Informe: %s\n", out)
	}
	if rep.OK {
		fmt.Println("\nResultado: OK")
	} else {
		fmt.Println("\nResultado: FALLA")
		for _, c := range rep.Checks {
			if !c.OK && c.Error != "" {
				fmt.Printf("  - %s: %s\n", c.Name, c.Error)
			}
		}
	}
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "s", "si", "sí", "y", "yes":
		return true
	}
	return false
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "verificar: "+format+"\n", args...)
	os.Exit(1)
}
