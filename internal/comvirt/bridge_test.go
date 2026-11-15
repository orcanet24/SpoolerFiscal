package comvirt

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/frame"
	"spoolerfiscal/internal/testutil"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func bridgeConfig() config.Agent {
	cfg := config.DefaultAgent()
	cfg.StationID = "CAJA-01"
	cfg.Server.Token = "x"
	cfg.Bridge.ReadGapMS = 20
	cfg.Bridge.ResponseTimeoutMS = 2000
	cfg.Bridge.MaxFrameBytes = 4096
	return cfg
}

func TestBridgeForwardsBytesVerbatim(t *testing.T) {
	host, agentSide := testutil.Pipe()
	cfg := bridgeConfig()
	bridge, err := NewBridge(agentSide, func(_ context.Context, req []byte) ([]byte, error) {
		return append([]byte("R:"), req...), nil
	}, cfg, testLogger())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bridge.Run(ctx) }()

	if err := frame.Write(host, []byte("101\r\n")); err != nil {
		t.Fatalf("escribir comando: %v", err)
	}
	resp, err := frame.Read(ctx, host, 20*time.Millisecond, 2*time.Second, 4096)
	if err != nil {
		t.Fatalf("leer respuesta: %v", err)
	}
	if string(resp) != "R:101\r\n" {
		t.Fatalf("respuesta = %q", resp)
	}

	// Varias tramas seguidas sobre el mismo puerto.
	for _, cmd := range []string{"S1\r\n", "U0X\r\n"} {
		if err := frame.Write(host, []byte(cmd)); err != nil {
			t.Fatalf("escribir %q: %v", cmd, err)
		}
		resp, err := frame.Read(ctx, host, 20*time.Millisecond, 2*time.Second, 4096)
		if err != nil {
			t.Fatalf("leer %q: %v", cmd, err)
		}
		if string(resp) != "R:"+cmd {
			t.Fatalf("respuesta = %q", resp)
		}
	}
}

func TestBridgeWritesConfiguredOfflineResponse(t *testing.T) {
	host, agentSide := testutil.Pipe()
	cfg := bridgeConfig()
	cfg.Bridge.OfflineResponseHex = "0x15,0x00"
	bridge, err := NewBridge(agentSide, func(context.Context, []byte) ([]byte, error) {
		return nil, errors.New("spooler inalcanzable")
	}, cfg, testLogger())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bridge.Run(ctx) }()

	if err := frame.Write(host, []byte("S1\r\n")); err != nil {
		t.Fatalf("escribir: %v", err)
	}
	resp, err := frame.Read(ctx, host, 20*time.Millisecond, 2*time.Second, 4096)
	if err != nil {
		t.Fatalf("leer: %v", err)
	}
	if len(resp) != 2 || resp[0] != 0x15 || resp[1] != 0x00 {
		t.Fatalf("respuesta offline = %v", resp)
	}
	if bridge.Errors.Load() != 1 {
		t.Fatalf("errores = %d", bridge.Errors.Load())
	}
}

func TestParseHexBytes(t *testing.T) {
	got, err := ParseHexBytes("0x1A 0B,03-ff")
	if err != nil {
		t.Fatalf("ParseHexBytes: %v", err)
	}
	want := []byte{0x1A, 0x0B, 0x03, 0xFF}
	if string(got) != string(want) {
		t.Fatalf("bytes = %v", got)
	}
	if _, err := ParseHexBytes("ZZ"); err == nil {
		t.Fatal("se esperaba error con hexadecimal inválido")
	}
}
