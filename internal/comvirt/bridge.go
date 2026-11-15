// Package comvirt implementa el puente software entre el puerto COM virtual que
// ve Profit Plus y el agente que habla con el spooler. El puente no interpreta
// el protocolo fiscal: delimita tramas por silencio, las reenvía y devuelve la
// respuesta real byte a byte.
package comvirt

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/fiscal"
	"spoolerfiscal/internal/frame"
)

// Bridge copia tramas entre el COM virtual y el manejador remoto.
type Bridge struct {
	Port            frame.Stream
	Handle          func(context.Context, []byte) ([]byte, error)
	Gap             time.Duration
	ResponseTimeout time.Duration
	MaxFrameBytes   int
	WriteSettle     time.Duration
	OfflineResponse []byte
	Log             *slog.Logger
	LogHex          bool

	Frames atomic.Uint64
	Errors atomic.Uint64
}

// NewBridge construye el puente con la configuración del agente.
func NewBridge(port frame.Stream, handle func(context.Context, []byte) ([]byte, error), cfg config.Agent, log *slog.Logger) (*Bridge, error) {
	if log == nil {
		log = slog.Default()
	}
	offline, err := ParseHexBytes(cfg.Bridge.OfflineResponseHex)
	if err != nil {
		return nil, err
	}
	gap := time.Duration(cfg.Bridge.ReadGapMS) * time.Millisecond
	if gap <= 0 {
		gap = 30 * time.Millisecond
	}
	timeout := time.Duration(cfg.Bridge.ResponseTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	maxBytes := cfg.Bridge.MaxFrameBytes
	if maxBytes <= 0 {
		maxBytes = 16 * 1024
	}
	return &Bridge{
		Port: port, Handle: handle, Gap: gap, ResponseTimeout: timeout,
		MaxFrameBytes:   maxBytes,
		WriteSettle:     time.Duration(cfg.Bridge.WriteSettleMS) * time.Millisecond,
		OfflineResponse: offline,
		Log:             log,
		LogHex:          cfg.Bridge.LogHexPayloads,
	}, nil
}

// Run atiende el puerto hasta que el contexto se cancele o el puerto falle.
// Un error de puerto (cable/COM cerrado) se devuelve para que el servicio
// vuelva a abrirlo; los silencios normales no son errores.
func (b *Bridge) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		req, err := frame.Read(ctx, b.Port, b.Gap, b.ResponseTimeout, b.MaxFrameBytes)
		if err != nil {
			switch {
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return nil
			case errors.Is(err, frame.ErrTimeout):
				// Profit Plus no envió nada en este intervalo: seguir esperando.
				continue
			case errors.Is(err, frame.ErrTooLarge):
				b.Errors.Add(1)
				b.Log.Error("trama del COM virtual excede el máximo; se descarta",
					"max_bytes", b.MaxFrameBytes)
				continue
			default:
				return err
			}
		}
		if len(req) == 0 {
			continue
		}

		b.Frames.Add(1)
		if b.LogHex {
			b.Log.Debug("comando desde Profit Plus", "bytes", hex.EncodeToString(req))
		}

		resp, herr := b.Handle(ctx, req)
		if herr != nil {
			b.Errors.Add(1)
			b.Log.Error("no se pudo obtener respuesta del spooler",
				"error", herr, "comando", printable(req))
			// Por omisión no se inventa una respuesta fiscal: Profit Plus debe
			// ver un timeout de comunicaciones. Sólo se envía algo si el
			// operador configuró offline_response_hex.
			if len(b.OfflineResponse) > 0 {
				if werr := frame.WriteSettled(b.Port, b.OfflineResponse, b.WriteSettle); werr != nil {
					return werr
				}
			}
			continue
		}
		if b.LogHex {
			b.Log.Debug("respuesta de la impresora", "bytes", hex.EncodeToString(resp))
		}
		if err := frame.WriteSettled(b.Port, resp, b.WriteSettle); err != nil {
			return fmt.Errorf("escribir respuesta en el COM virtual: %w", err)
		}
	}
}

// OpenPort abre el extremo del par COM que corresponde al agente.
func OpenPort(p config.Port) (frame.Stream, error) {
	params, err := config.ParsePort(p)
	if err != nil {
		return nil, err
	}
	return fiscal.OpenSerial(fiscal.SerialConfig{
		Name: p.Name, BaudRate: params.BaudRate, DataBits: params.DataBits,
		Parity: params.Parity, StopBits: params.StopBits, FlowControl: params.FlowControl,
	})
}

// ParseHexBytes convierte "0x1A,0x02" o "1A 02" en bytes.
func ParseHexBytes(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	s = strings.ReplaceAll(s, "0x", "")
	s = strings.ReplaceAll(s, "0X", "")
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '-' || r == ':'
	})
	out := make([]byte, 0, len(fields))
	for _, f := range fields {
		v, err := strconv.ParseUint(f, 16, 8)
		if err != nil {
			return nil, fmt.Errorf("byte hexadecimal inválido %q en offline_response_hex", f)
		}
		out = append(out, byte(v))
	}
	return out, nil
}

func printable(b []byte) string {
	out := make([]rune, 0, len(b))
	for _, c := range b {
		if c >= 32 && c < 127 {
			out = append(out, rune(c))
		} else {
			out = append(out, '.')
		}
	}
	return string(out)
}
