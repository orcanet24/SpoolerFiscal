package frame

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeStream struct {
	mu      sync.Mutex
	chunks  [][]byte
	written [][]byte
}

func (f *fakeStream) Read(p []byte) (int, error) {
	f.mu.Lock()
	if len(f.chunks) > 0 {
		chunk := f.chunks[0]
		n := copy(p, chunk)
		if n == len(chunk) {
			f.chunks = f.chunks[1:]
		} else {
			f.chunks[0] = chunk[n:]
		}
		f.mu.Unlock()
		return n, nil
	}
	f.mu.Unlock()
	time.Sleep(time.Millisecond)
	return 0, nil
}

func (f *fakeStream) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.written = append(f.written, append([]byte(nil), p...))
	f.mu.Unlock()
	return len(p), nil
}

func (f *fakeStream) Close() error                       { return nil }
func (f *fakeStream) SetReadTimeout(time.Duration) error { return nil }

func TestReadReturnsWholeFrameAfterSilence(t *testing.T) {
	s := &fakeStream{chunks: [][]byte{[]byte("S1\r\n")}}
	start := time.Now()
	got, err := Read(context.Background(), s, 20*time.Millisecond, time.Second, 1024)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "S1\r\n" {
		t.Fatalf("trama = %q, se esperaba %q", got, "S1\r\n")
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("la trama se cerró antes del silencio configurado (%v)", elapsed)
	}
}

func TestReadKeepsBytesVerbatimIncludingNUL(t *testing.T) {
	payload := []byte{0x00, 0x1B, 0x40, 0xFF, 0x0D}
	s := &fakeStream{chunks: [][]byte{payload}}
	got, err := Read(context.Background(), s, 10*time.Millisecond, time.Second, 1024)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("bytes alterados: %v != %v", got, payload)
	}
}

func TestReadTimesOutWithoutData(t *testing.T) {
	s := &fakeStream{}
	_, err := Read(context.Background(), s, 5*time.Millisecond, 40*time.Millisecond, 1024)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("se esperaba ErrTimeout, se obtuvo %v", err)
	}
}

func TestReadRejectsOversizedFrame(t *testing.T) {
	s := &fakeStream{chunks: [][]byte{make([]byte, 64)}}
	_, err := Read(context.Background(), s, 5*time.Millisecond, time.Second, 16)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("se esperaba ErrTooLarge, se obtuvo %v", err)
	}
}

func TestWriteSendsAllBytes(t *testing.T) {
	s := &fakeStream{}
	if err := Write(s, []byte("101\r")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.written) != 1 || string(s.written[0]) != "101\r" {
		t.Fatalf("escrituras = %q", s.written)
	}
}
