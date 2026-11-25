package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExampleServerConfig(t *testing.T) {
	cfg, err := LoadServer(filepath.Join("..", "..", "configs", "server.yaml"))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Printer.Driver != "tfhka" {
		t.Fatalf("driver = %q", cfg.Printer.Driver)
	}
	if cfg.Printer.Port.BaudRate != 9600 {
		t.Fatalf("baud_rate = %d", cfg.Printer.Port.BaudRate)
	}
	if cfg.Queue.MaxPending != 256 {
		t.Fatalf("max_pending = %d", cfg.Queue.MaxPending)
	}
	if !cfg.Guard.BlockOnPaperOut {
		t.Fatal("se esperaba block_on_paper_out=true")
	}
	if cfg.Printer.CommandTerminator != "\r\n" {
		t.Fatalf("terminador = %q", cfg.Printer.CommandTerminator)
	}
}

func TestValidateAcceptsImplementedDrivers(t *testing.T) {
	for _, driver := range []string{"tfhka", "hasar", "bixolon", "epson", ""} {
		cfg := DefaultServer()
		cfg.Token = "token"
		cfg.Printer.Driver = driver
		cfg.Printer.Transport = "tcp"
		cfg.Printer.TCPAddress = "127.0.0.1:2011"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("driver %q: %v", driver, err)
		}
	}

	cfg := DefaultServer()
	cfg.Token = "token"
	cfg.Printer.Driver = "ncr"
	cfg.Printer.Transport = "tcp"
	cfg.Printer.TCPAddress = "127.0.0.1:2011"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "no soportado") {
		t.Fatalf("driver no implementado aceptado: %v", err)
	}
}

// Un YAML con driver IxBatch debe cargar y validar sin tocar el resto.
func TestLoadServerWithIxBatchDriver(t *testing.T) {
	dir := t.TempDir()
	db := filepath.ToSlash(filepath.Join(dir, "spooler.db"))
	path := filepath.Join(dir, "server.yaml")
	yaml := "token: token-ix\n" +
		"database:\n  path: " + db + "\n" +
		"http:\n  listen: 127.0.0.1:0\n" +
		"printer:\n  driver: hasar\n  transport: tcp\n  tcp_address: 127.0.0.1:2011\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Printer.Driver != "hasar" {
		t.Fatalf("driver = %q", cfg.Printer.Driver)
	}
	if cfg.Queue.MaxPending != 256 {
		t.Fatalf("los defaults se perdieron: max_pending = %d", cfg.Queue.MaxPending)
	}
	if cfg.Printer.StatusProbe != "S2" {
		t.Fatalf("status_probe = %q", cfg.Printer.StatusProbe)
	}
}

func TestValidateStationTokens(t *testing.T) {
	base := func() Server {
		s := DefaultServer()
		s.Token = "compartido"
		s.Printer.Transport = "tcp"
		s.Printer.TCPAddress = "127.0.0.1:2011"
		return s
	}
	valid := base()
	valid.StationTokens = map[string]string{"CAJA-01": "token-1", "CAJA-02": "token-2"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("tokens válidos rechazados: %v", err)
	}

	cases := []struct {
		name   string
		tokens map[string]string
	}{
		{"token compartido", map[string]string{"CAJA-01": "compartido"}},
		{"token duplicado", map[string]string{"CAJA-01": "t", "CAJA-02": "t"}},
		{"estación inválida", map[string]string{"CAJA 01": "t"}},
		{"token vacío", map[string]string{"CAJA-01": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			s.StationTokens = tc.tokens
			if err := s.Validate(); err == nil {
				t.Fatalf("debía rechazarse: %v", tc.tokens)
			}
		})
	}
}

func TestValidateAgentTLSPair(t *testing.T) {
	a := DefaultAgent()
	a.StationID = "CAJA-01"
	a.Server.Token = "t"
	a.Server.Address = "https://127.0.0.1:8080"
	a.VirtualCOM.Name = "COM4"

	a.Server.TLS = TLS{Enabled: true, CertFile: "cliente.pem"}
	if err := a.Validate(); err == nil {
		t.Fatal("cert_file sin key_file debía rechazarse")
	}
	a.Server.TLS = TLS{Enabled: true, CertFile: "cliente.pem", KeyFile: "cliente.key"}
	if err := a.Validate(); err != nil {
		t.Fatalf("par cert/key válido rechazado: %v", err)
	}
}

func TestLoadExampleAgentConfig(t *testing.T) {
	cfg, err := LoadAgent(filepath.Join("..", "..", "configs", "agent.yaml"))
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if cfg.StationID != "CAJA-01" {
		t.Fatalf("station_id = %q", cfg.StationID)
	}
	if cfg.VirtualCOM.Name != "COM4" {
		t.Fatalf("puerto virtual = %q", cfg.VirtualCOM.Name)
	}
	if cfg.Server.Address != "http://192.168.1.10:8080" {
		t.Fatalf("address = %q", cfg.Server.Address)
	}
}

func TestAgentRequiresStationAndToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte("server:\n  address: http://127.0.0.1:8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAgent(path)
	if err == nil {
		t.Fatal("se esperaba error de validación")
	}
	msg := err.Error()
	if !strings.Contains(msg, "station_id") || !strings.Contains(msg, "token") {
		t.Fatalf("mensaje poco claro: %v", err)
	}
}

func TestUnknownKeyIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	content := "token: x\ncampo_inexistente: 1\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServer(path); err == nil {
		t.Fatal("se esperaba error por clave desconocida")
	}
}

func TestTokenFromEnvironment(t *testing.T) {
	t.Setenv("SPOOLER_TOKEN", "token-de-entorno")
	cfg := DefaultServer()
	cfg.HTTP.Listen = "127.0.0.1:0"
	cfg.Database.Path = "data/x.db"
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(path, []byte("http:\n  listen: 127.0.0.1:8080\ndatabase:\n  path: data/x.db\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if loaded.Token != "token-de-entorno" {
		t.Fatalf("token = %q", loaded.Token)
	}
}

func TestParsePortValidation(t *testing.T) {
	if _, err := ParsePort(Port{BaudRate: 9600, DataBits: 8, Parity: "none", StopBits: "1"}); err != nil {
		t.Fatalf("8N1 debería ser válido: %v", err)
	}
	if _, err := ParsePort(Port{DataBits: 8, Parity: "inventada"}); err == nil {
		t.Fatal("paridad inválida aceptada")
	}
	if _, err := ParsePort(Port{DataBits: 9}); err == nil {
		t.Fatal("data_bits inválido aceptado")
	}
	if _, err := ParsePort(Port{FlowControl: "magia"}); err == nil {
		t.Fatal("flow_control inválido aceptado")
	}
}

func TestServerRejectsUnknownTransport(t *testing.T) {
	cfg := DefaultServer()
	cfg.Token = "x"
	cfg.Database.Path = "data/x.db"
	cfg.Printer.Transport = "usb"
	if err := cfg.Validate(); err == nil {
		t.Fatal("transporte desconocido aceptado")
	}
}
