// Package frame delimita tramas sobre un flujo serie sin interpretar su
// contenido. Una trama termina cuando el emisor guarda silencio durante un
// intervalo configurable (gap inter-byte), que es como las impresoras fiscales
// y los drivers de Profit Plus separan comando de respuesta.
package frame

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	// ErrTimeout indica que no llegó ningún byte antes del vencimiento total.
	ErrTimeout = errors.New("frame: sin respuesta dentro del tiempo configurado")
	// ErrTooLarge indica una trama mayor al máximo permitido.
	ErrTooLarge = errors.New("frame: trama mayor que el máximo permitido")
)

// Stream es un flujo bidireccional con timeout de lectura configurable.
// Lo implementan el puerto serie físico, el COM virtual y el transporte TCP.
type Stream interface {
	io.ReadWriteCloser
	SetReadTimeout(time.Duration) error
}

// Read acumula bytes hasta que el emisor queda en silencio `gap`, hasta llegar
// a `maxBytes` o hasta vencer `total`. Devuelve los bytes tal cual llegaron:
// el framing nunca modifica el contenido.
func Read(ctx context.Context, s Stream, gap, total time.Duration, maxBytes int) ([]byte, error) {
	if s == nil {
		return nil, errors.New("frame: stream nulo")
	}
	if gap <= 0 {
		gap = 20 * time.Millisecond
	}
	if total <= 0 {
		total = 5 * time.Second
	}
	if maxBytes <= 0 {
		maxBytes = 64 * 1024
	}

	buf := make([]byte, 0, 512)
	tmp := make([]byte, 1024)
	deadline := time.Now().Add(total)
	var lastByte time.Time

	for {
		// El plazo total se evalúa primero: su vencimiento es "sin respuesta"
		// (ErrTimeout), no una cancelación externa. Evaluar ctx primero
		// devolvía DeadlineExceeded en la frontera exacta — y el puente COM
		// moría en un intervalo silencioso en vez de seguir escuchando.
		now := time.Now()
		if !now.Before(deadline) {
			if len(buf) > 0 {
				return buf, nil
			}
			return nil, ErrTimeout
		}
		if err := ctx.Err(); err != nil {
			if len(buf) > 0 {
				return buf, nil
			}
			return nil, err
		}
		if len(buf) > 0 && now.Sub(lastByte) >= gap {
			return buf, nil
		}

		tick := gap
		if remain := time.Until(deadline); remain < tick {
			tick = remain
		}
		if len(buf) > 0 {
			if remain := gap - now.Sub(lastByte); remain > 0 && remain < tick {
				tick = remain
			}
		}
		if tick < time.Millisecond {
			tick = time.Millisecond
		}
		if err := s.SetReadTimeout(tick); err != nil {
			return nil, fmt.Errorf("frame: timeout de lectura: %w", err)
		}

		n, err := s.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			lastByte = time.Now()
			if len(buf) > maxBytes {
				return nil, ErrTooLarge
			}
			if len(buf) >= maxBytes {
				return buf, nil
			}
			continue
		}
		if err != nil {
			// Un error con datos ya acumulados cierra la trama: la respuesta
			// parcial es válida para el protocolo.
			if len(buf) > 0 {
				return buf, nil
			}
			return nil, err
		}
		// Sin bytes: el timeout parcial venció (silencio del emisor).
	}
}

// Write envía todos los bytes, reintentando escrituras parciales.
func Write(s Stream, p []byte) error {
	if s == nil {
		return errors.New("frame: stream nulo")
	}
	for len(p) > 0 {
		n, err := s.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// WriteSettled escribe la trama y espera `settle` antes de permitir la lectura.
// Algunas impresoras fiscales necesitan ese margen para procesar el comando.
func WriteSettled(s Stream, p []byte, settle time.Duration) error {
	if err := Write(s, p); err != nil {
		return err
	}
	if settle > 0 {
		time.Sleep(settle)
	}
	return nil
}
