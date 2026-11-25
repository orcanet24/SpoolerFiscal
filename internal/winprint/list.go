//go:build windows

package winprint

import (
	"fmt"
	"unicode/utf16"
	"unsafe"
)

// EnumPrinters: sólo impresoras locales y conexiones de red del usuario.
const (
	printerEnumLocal       = 0x00000002
	printerEnumConnections = 0x00000004
)

var procEnumPrintersW = winspool.NewProc("EnumPrintersW")

// printerInfo4 es PRINTER_INFO_4W: nombre, servidor y atributos.
type printerInfo4 struct {
	pPrinterName *uint16
	pServerName  *uint16
	attributes   uint32
}

// List devuelve los nombres de las impresoras instaladas en Windows (colas
// locales y conexiones de red). Sirve para localizar una térmica o una
// fiscal que el fabricante expone como impresora de Windows en lugar de COM.
func List() ([]string, error) {
	flags := uintptr(printerEnumLocal | printerEnumConnections)
	var needed, returned uint32

	// Primera llamada: sólo el tamaño de buffer necesario.
	r, _, callErr := procEnumPrintersW.Call(
		flags, 0, 4, 0, 0,
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r == 0 && needed == 0 {
		return nil, fmt.Errorf("winprint: EnumPrinters: %w", callErr)
	}
	if needed == 0 {
		return nil, nil
	}

	buf := make([]byte, needed)
	r, _, callErr = procEnumPrintersW.Call(
		flags, 0, 4,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r == 0 {
		return nil, fmt.Errorf("winprint: EnumPrinters: %w", callErr)
	}

	infos := unsafe.Slice((*printerInfo4)(unsafe.Pointer(&buf[0])), int(returned))
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		if name := utf16PtrToString(info.pPrinterName); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// utf16PtrToString convierte una cadena UTF-16 terminada en NUL. No se puede
// usar syscall.UTF16ToString con un puntero suelto sin una longitud conocida.
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	out := make([]uint16, 0, 32)
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		v := *(*uint16)(ptr)
		if v == 0 {
			break
		}
		out = append(out, v)
	}
	return string(utf16.Decode(out))
}
