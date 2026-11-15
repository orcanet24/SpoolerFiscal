//go:build windows

// Package winprint envía bytes ESC/POS crudos a una impresora instalada en
// Windows (cola USB00x) usando el spooler (winspool.drv) en modo RAW. Es la
// vía para térmicas como la Xprinter XP-80 conectadas por USB cuando no
// exponen puerto COM ni red.
package winprint

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	winspool = syscall.NewLazyDLL("winspool.drv")

	procOpenPrinterW     = winspool.NewProc("OpenPrinterW")
	procStartDocPrinterW = winspool.NewProc("StartDocPrinterW")
	procWritePrinter     = winspool.NewProc("WritePrinter")
	procEndDocPrinter    = winspool.NewProc("EndDocPrinter")
	procClosePrinter     = winspool.NewProc("ClosePrinter")
)

// docInfo1 es DOC_INFO_1 de winspool: nombre, archivo de salida (nulo) y tipo
// de datos RAW (bytes sin procesar por el driver).
type docInfo1 struct {
	pDocName    *uint16
	pOutputFile *uint16
	pDatatype   *uint16
}

// Print envía data (bytes ESC/POS) a la impresora name como documento RAW.
// El documento se llama "SpoolerFiscal (prueba, no fiscal)".
func Print(printerName string, data []byte) error {
	if printerName == "" {
		return fmt.Errorf("winprint: nombre de impresora vacío")
	}
	if len(data) == 0 {
		return fmt.Errorf("winprint: no hay datos que imprimir")
	}
	h, err := open(printerName)
	if err != nil {
		return err
	}
	defer closePrinter(h)

	docName, err := syscall.UTF16PtrFromString("SpoolerFiscal (prueba, no fiscal)")
	if err != nil {
		return err
	}
	datatype, err := syscall.UTF16PtrFromString("RAW")
	if err != nil {
		return err
	}
	info := docInfo1{pDocName: docName, pDatatype: datatype}

	r, _, callErr := procStartDocPrinterW.Call(h, 1, uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return fmt.Errorf("winprint: StartDocPrinter %s: %w", printerName, callErr)
	}

	var written uint32
	r, _, callErr = procWritePrinter.Call(
		h,
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&written)),
	)
	if r == 0 {
		_, _, _ = procEndDocPrinter.Call(h)
		return fmt.Errorf("winprint: WritePrinter: %w", callErr)
	}
	if written != uint32(len(data)) {
		_, _, _ = procEndDocPrinter.Call(h)
		return fmt.Errorf("winprint: WritePrinter: solo %d de %d bytes", written, len(data))
	}
	if r, _, callErr = procEndDocPrinter.Call(h); r == 0 {
		return fmt.Errorf("winprint: EndDocPrinter: %w", callErr)
	}
	return nil
}

func open(printerName string) (uintptr, error) {
	name, err := syscall.UTF16PtrFromString(printerName)
	if err != nil {
		return 0, err
	}
	var h uintptr
	r, _, callErr := procOpenPrinterW.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&h)), 0)
	if r == 0 {
		return 0, fmt.Errorf("winprint: abrir %s: %w (¿existe la impresora?)", printerName, callErr)
	}
	return h, nil
}

func closePrinter(h uintptr) {
	_, _, _ = procClosePrinter.Call(h)
}
