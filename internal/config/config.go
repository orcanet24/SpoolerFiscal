// Package config carga y valida la configuración YAML del agente y del
// servidor. Los valores ausentes toman los defaults del archivo de ejemplo.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"spoolerfiscal/internal/fiscal"
)

// Logging describe el destino y la rotación de logs.
type Logging struct {
	Level      string `yaml:"level"`
	File       string `yaml:"file"`
	Console    bool   `yaml:"console"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxBackups int    `yaml:"max_backups"`
	MaxAgeDays int    `yaml:"max_age_days"`
	Compress   bool   `yaml:"compress"`
}

// Port son los parámetros del puerto serie (físico o virtual).
type Port struct {
	Name        string `yaml:"name"`
	BaudRate    int    `yaml:"baud_rate"`
	DataBits    int    `yaml:"data_bits"`
	Parity      string `yaml:"parity"`
	StopBits    string `yaml:"stop_bits"`
	FlowControl string `yaml:"flow_control"`
}

// TLS habilita mTLS opcional entre agente y servidor.
type TLS struct {
	Enabled  bool   `yaml:"enabled"`
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// ServerEndpoint es la conexión del agente hacia la API del spooler.
type ServerEndpoint struct {
	Address          string `yaml:"address"`
	Token            string `yaml:"token"`
	TLS              TLS    `yaml:"tls"`
	RequestTimeoutMS int    `yaml:"request_timeout_ms"`
}

// Bridge controla el puente de bytes entre el COM virtual y la API HTTP.
type Bridge struct {
	ReadGapMS           int    `yaml:"read_gap_ms"`
	ResponseTimeoutMS   int    `yaml:"response_timeout_ms"`
	MaxFrameBytes       int    `yaml:"max_frame_bytes"`
	WriteSettleMS       int    `yaml:"write_settle_ms"`
	OfflineResponseHex  string `yaml:"offline_response_hex"`
	ReopenDelayMS       int    `yaml:"reopen_delay_ms"`
	ReopenMaxDelayMS    int    `yaml:"reopen_max_delay_ms"`
	LocalQueueSize      int    `yaml:"local_queue_size"`
	QueueFrameTimeoutMS int    `yaml:"queue_frame_timeout_ms"`
	MaxSendBackoffMS    int    `yaml:"max_send_backoff_ms"`
	LogHexPayloads      bool   `yaml:"log_hex_payloads"`
}

// Agent es la configuración completa del servicio de estación.
type Agent struct {
	StationID   string         `yaml:"station_id"`
	Server      ServerEndpoint `yaml:"server"`
	VirtualCOM  Port           `yaml:"virtual_com"`
	Bridge      Bridge         `yaml:"bridge"`
	Logging     Logging        `yaml:"logging"`
	ConfigPath  string         `yaml:"-"`
	ServiceName string         `yaml:"-"`
}

// Printer es la impresora física del servidor.
type PrinterCfg struct {
	Driver            string `yaml:"driver"`
	Transport         string `yaml:"transport"`
	Port              Port   `yaml:"port"`
	TCPAddress        string `yaml:"tcp_address"`
	TCPDialTimeoutMS  int    `yaml:"tcp_dial_timeout_ms"`
	ReadGapMS         int    `yaml:"read_gap_ms"`
	ResponseTimeoutMS int    `yaml:"response_timeout_ms"`
	MaxResponseBytes  int    `yaml:"max_response_bytes"`
	WriteSettleMS     int    `yaml:"write_settle_ms"`
	StatusProbe       string `yaml:"status_probe"`
	CommandTerminator string `yaml:"command_terminator"`
	OpenRetryMS       int    `yaml:"open_retry_ms"`
}

// Queue controla la cola única serializada del spooler.
type Queue struct {
	MaxPending       int `yaml:"max_pending"`
	DefaultTimeoutMS int `yaml:"default_timeout_ms"`
	MaxTimeoutMS     int `yaml:"max_timeout_ms"`
	ProbeTimeoutMS   int `yaml:"probe_timeout_ms"`
	// Libera el turno de una factura abandonada (Profit Plus cerrado a mitad de
	// emisión). 0 = no liberar nunca; requiere intervención del operador.
	DocumentLockTimeoutMS int `yaml:"document_lock_timeout_ms"`
	// Edad máxima del correlativo conciliado (S1) antes de que el spooler lo
	// re-consulte antes de abrir una factura. La impresora es la fuente legal.
	CorrelativeSyncMaxAgeMS int `yaml:"correlative_sync_max_age_ms"`
	// Sonda de estado periódica cuando la cola está vacía: mantiene fresco el
	// guardián (papel, memoria, documento colgado). 0 = desactivada.
	StatusProbeIntervalSeconds int `yaml:"status_probe_interval_seconds"`
	// Permite enviar el cierre 101 de recuperación cuando la impresora tiene un
	// documento abierto que ninguna sesión reclama.
	AutoCloseOpenDocument *bool `yaml:"auto_close_open_document"`
}

// Guard bloquea emisiones cuando la impresora no está en el estado esperado.
type Guard struct {
	BlockOnPaperOut   bool `yaml:"block_on_paper_out"`
	BlockOnMemoryFull bool `yaml:"block_on_memory_full"`
	BlockOnError      bool `yaml:"block_on_error"`
	BlockWhenOffline  bool `yaml:"block_when_offline"`
}

// Database es la persistencia embebida (SQLite, driver puro Go).
type Database struct {
	Path          string `yaml:"path"`
	LogCommands   bool   `yaml:"log_commands"`
	RetentionDays int    `yaml:"retention_days"`
}

// Metrics define el volcado periódico de métricas al log.
type Metrics struct {
	IntervalSeconds int `yaml:"interval_seconds"`
}

// HTTP es la API del spooler: la usan los agentes de estación y los sistemas
// externos (ecommerce, ERP, CRM). Es el único transporte de entrada.
type HTTP struct {
	Listen                string `yaml:"listen"`
	MaxBodyBytes          int64  `yaml:"max_body_bytes"`
	ReadTimeoutMS         int    `yaml:"read_timeout_ms"`
	WriteTimeoutMS        int    `yaml:"write_timeout_ms"`
	IdempotencyRetentionD int    `yaml:"idempotency_retention_days"`
	// Frecuencia del despachador que reanuda trabajos pausados y ejecuta los
	// asíncronos de la cola duradera. 0 = 10 segundos.
	JobResumeIntervalSeconds int `yaml:"job_resume_interval_seconds"`
	// Habilita/deshabilita el servidor HTTP (compatibilidad con configs legacy)
	Enabled *bool `yaml:"enabled"`
}

// Server es la configuración completa del servicio central.
type Server struct {
	Token string `yaml:"token"`
	// StationTokens ata una estación a su propio token. Si una estación tiene
	// token propio, el token compartido deja de representarla: sólo su token
	// puede emitir en su nombre. Vacío = todas usan el token compartido.
	StationTokens   map[string]string `yaml:"station_tokens"`
	TLS             TLS               `yaml:"tls"`
	AllowedStations []string          `yaml:"allowed_stations"`
	Printer         PrinterCfg        `yaml:"printer"`
	Queue           Queue             `yaml:"queue"`
	Guard           Guard             `yaml:"guard"`
	Database        Database          `yaml:"database"`
	Logging         Logging           `yaml:"logging"`
	Metrics         Metrics           `yaml:"metrics"`
	HTTP            HTTP              `yaml:"http"`
	ConfigPath      string            `yaml:"-"`
	ServiceName     string            `yaml:"-"`
}

// ValidStationID indica si un identificador de estación es aceptable: entre 1
// y 64 caracteres, letras, números y - _ . :
func ValidStationID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == ':'
		if !ok {
			return false
		}
	}
	return true
}

// validateStationTokens revisa los tokens por estación: identificador válido,
// token no vacío, sin repetir token (dos estaciones con el mismo token serían
// indistinguibles) y distinto del token compartido.
func validateStationTokens(s Server) error {
	seen := make(map[string]string, len(s.StationTokens))
	for station, token := range s.StationTokens {
		if !ValidStationID(station) {
			return fmt.Errorf("station_tokens: identificador de estación %q inválido", station)
		}
		tok := strings.TrimSpace(token)
		if tok == "" {
			return fmt.Errorf("station_tokens[%s]: el token no puede estar vacío", station)
		}
		if s.Token != "" && tok == s.Token {
			return fmt.Errorf("station_tokens[%s]: no puede usar el token compartido", station)
		}
		if prev, dup := seen[tok]; dup {
			return fmt.Errorf("station_tokens[%s]: comparte token con la estación %q", station, prev)
		}
		seen[tok] = station
	}
	return nil
}

func defaultsLogging(file string) Logging {
	return Logging{
		Level: "info", File: file, Console: true,
		MaxSizeMB: 20, MaxBackups: 10, MaxAgeDays: 30, Compress: true,
	}
}

// DefaultAgent devuelve la configuración base del agente.
func DefaultAgent() Agent {
	return Agent{
		StationID: "",
		Server: ServerEndpoint{
			Address:          "http://127.0.0.1:8080",
			RequestTimeoutMS: 30000,
		},
		VirtualCOM: Port{
			Name: "COM4",
			// El COM virtual replica los parámetros reales de la impresora:
			// Profit Plus abre el otro extremo del par con los mismos ajustes.
			BaudRate: 9600, DataBits: 8, Parity: "none", StopBits: "1", FlowControl: "none",
		},
		Bridge: Bridge{
			ReadGapMS: 30, ResponseTimeoutMS: 15000, MaxFrameBytes: 16384,
			WriteSettleMS: 0, ReopenDelayMS: 1000, ReopenMaxDelayMS: 30000,
			LocalQueueSize: 16, QueueFrameTimeoutMS: 30000, MaxSendBackoffMS: 2000,
			LogHexPayloads: false,
		},
		Logging: defaultsLogging("logs/agent.log"),
	}
}

// DefaultServer devuelve la configuración base del servidor.
func DefaultServer() Server {
	return Server{
		AllowedStations: nil,
		Printer: PrinterCfg{
			Driver: "tfhka", Transport: "serial",
			Port:             Port{Name: "COM1", BaudRate: 9600, DataBits: 8, Parity: "none", StopBits: "1", FlowControl: "none"},
			TCPDialTimeoutMS: 3000, ReadGapMS: 30, ResponseTimeoutMS: 20000,
			MaxResponseBytes: 32768, WriteSettleMS: 0,
			StatusProbe: "S2", CommandTerminator: "\r\n", OpenRetryMS: 500,
		},
		Queue: Queue{
			MaxPending: 256, DefaultTimeoutMS: 30000, MaxTimeoutMS: 120000, ProbeTimeoutMS: 5000,
			DocumentLockTimeoutMS:      120000,
			CorrelativeSyncMaxAgeMS:    10000,
			StatusProbeIntervalSeconds: 15,
			AutoCloseOpenDocument:      boolPtr(true),
		},
		Guard: Guard{
			BlockOnPaperOut: true, BlockOnMemoryFull: true, BlockOnError: true,
			BlockWhenOffline: false,
		},
		Database: Database{Path: "data/spooler.db", LogCommands: true, RetentionDays: 365},
		Logging:  defaultsLogging("logs/server.log"),
		Metrics:  Metrics{IntervalSeconds: 60},
		HTTP: HTTP{
			Listen: "0.0.0.0:8080", MaxBodyBytes: 256 * 1024,
			ReadTimeoutMS: 15000, WriteTimeoutMS: 60000, IdempotencyRetentionD: 30,
			JobResumeIntervalSeconds: 10,
		},
	}
}

// LoadAgent lee el YAML del agente aplicando defaults y variables de entorno.
func LoadAgent(path string) (Agent, error) {
	cfg := DefaultAgent()
	if err := decode(path, &cfg); err != nil {
		return cfg, err
	}
	cfg.ConfigPath = path
	if v := os.Getenv("SPOOLER_TOKEN"); v != "" {
		cfg.Server.Token = v
	}
	if v := os.Getenv("SPOOLER_STATION_ID"); v != "" {
		cfg.StationID = v
	}
	if v := os.Getenv("SPOOLER_SERVER_ADDR"); v != "" {
		cfg.Server.Address = v
	}
	if v := os.Getenv("SPOOLER_VIRTUAL_COM"); v != "" {
		cfg.VirtualCOM.Name = v
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// LoadServer lee el YAML del servidor aplicando defaults y variables de entorno.
func LoadServer(path string) (Server, error) {
	cfg := DefaultServer()
	if err := decode(path, &cfg); err != nil {
		return cfg, err
	}
	cfg.ConfigPath = path
	if v := os.Getenv("SPOOLER_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("SPOOLER_PRINTER_PORT"); v != "" && cfg.Printer.Transport == "serial" {
		cfg.Printer.Port.Name = v
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func decode(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("leer configuración %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("YAML inválido en %s: %w", path, err)
	}
	return nil
}

// Validate revisa los campos que no pueden quedar vacíos.
func (a Agent) Validate() error {
	var errs []string
	if strings.TrimSpace(a.StationID) == "" {
		errs = append(errs, "station_id es obligatorio (identificador único de la estación)")
	}
	if strings.TrimSpace(a.Server.Address) == "" {
		errs = append(errs, "server.address es obligatorio (host:puerto del spooler)")
	}
	if strings.TrimSpace(a.Server.Token) == "" {
		errs = append(errs, "server.token es obligatorio (token compartido); puede venir de SPOOLER_TOKEN")
	}
	if strings.TrimSpace(a.VirtualCOM.Name) == "" {
		errs = append(errs, "virtual_com.name es obligatorio (ej. COM4)")
	}
	if a.Bridge.MaxFrameBytes <= 0 {
		errs = append(errs, "bridge.max_frame_bytes debe ser > 0")
	}
	if a.Bridge.LocalQueueSize <= 0 {
		errs = append(errs, "bridge.local_queue_size debe ser > 0")
	}
	if a.Logging.MaxAgeDays < 0 {
		errs = append(errs, "logging.max_age_days no puede ser negativo")
	}
	if _, err := ParsePort(a.VirtualCOM); err != nil {
		errs = append(errs, "virtual_com: "+err.Error())
	}
	if t := a.Server.TLS; t.Enabled && (t.CertFile == "") != (t.KeyFile == "") {
		errs = append(errs, "server.tls.cert_file y server.tls.key_file deben configurarse juntos (mTLS)")
	}
	if len(errs) > 0 {
		return errors.New("configuración del agente inválida:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// Validate revisa la configuración del servidor.
func (s Server) Validate() error {
	var errs []string
	if strings.TrimSpace(s.Token) == "" {
		errs = append(errs, "token es obligatorio (token compartido); puede venir de SPOOLER_TOKEN")
	}
	if err := validateStationTokens(s); err != nil {
		errs = append(errs, err.Error())
	}
	if strings.TrimSpace(s.Database.Path) == "" {
		errs = append(errs, "database.path es obligatorio")
	}
	if s.TLS.Enabled {
		if s.TLS.CertFile == "" || s.TLS.KeyFile == "" {
			errs = append(errs, "tls.cert_file y tls.key_file son obligatorios con tls.enabled=true")
		}
	}
	if a := s.TLS; a.Enabled && a.CAFile != "" && (a.CertFile == "" || a.KeyFile == "") {
		errs = append(errs, "tls.ca_file (mTLS) requiere tls.cert_file y tls.key_file")
	}
	switch strings.ToLower(strings.TrimSpace(s.Printer.Driver)) {
	case fiscal.BrandTFHKA, fiscal.BrandHasar, fiscal.BrandBixolon, fiscal.BrandEpson, "":
	default:
		errs = append(errs, fmt.Sprintf("printer.driver %q no soportado (soportados: %s)",
			s.Printer.Driver, strings.Join(fiscal.Drivers(), ", ")))
	}
	switch strings.ToLower(s.Printer.Transport) {
	case "serial":
		if _, err := ParsePort(s.Printer.Port); err != nil {
			errs = append(errs, "printer.port: "+err.Error())
		}
	case "tcp":
		if strings.TrimSpace(s.Printer.TCPAddress) == "" {
			errs = append(errs, "printer.tcp_address es obligatorio con transport=tcp")
		}
	default:
		errs = append(errs, fmt.Sprintf("printer.transport %q no soportado (serial|tcp)", s.Printer.Transport))
	}
	if strings.TrimSpace(s.HTTP.Listen) == "" {
		errs = append(errs, "http.listen es obligatorio (ej. 0.0.0.0:8080)")
	}
	if s.HTTP.MaxBodyBytes <= 0 {
		errs = append(errs, "http.max_body_bytes debe ser > 0")
	}
	if s.HTTP.ReadTimeoutMS <= 0 || s.HTTP.WriteTimeoutMS <= 0 {
		errs = append(errs, "http.read_timeout_ms y http.write_timeout_ms deben ser > 0")
	}
	if s.Queue.MaxPending <= 0 {
		errs = append(errs, "queue.max_pending debe ser > 0")
	}
	if s.Queue.CorrelativeSyncMaxAgeMS < 0 {
		errs = append(errs, "queue.correlative_sync_max_age_ms no puede ser negativo")
	}
	if s.Queue.StatusProbeIntervalSeconds < 0 {
		errs = append(errs, "queue.status_probe_interval_seconds no puede ser negativo")
	}
	if s.Printer.ResponseTimeoutMS <= 0 {
		errs = append(errs, "printer.response_timeout_ms debe ser > 0")
	}
	if len(errs) > 0 {
		return errors.New("configuración del servidor inválida:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// SerialParams son los parámetros normalizados del puerto serie.
type SerialParams struct {
	BaudRate    int
	DataBits    int
	Parity      string
	StopBits    string
	FlowControl string
}

// ParsePort valida y normaliza los parámetros serie.
func ParsePort(p Port) (SerialParams, error) {
	out := SerialParams{
		BaudRate: p.BaudRate, DataBits: p.DataBits,
		Parity:      strings.ToLower(strings.TrimSpace(p.Parity)),
		StopBits:    strings.TrimSpace(p.StopBits),
		FlowControl: strings.ToLower(strings.TrimSpace(p.FlowControl)),
	}
	if out.BaudRate <= 0 {
		out.BaudRate = 9600
	}
	if out.DataBits == 0 {
		out.DataBits = 8
	}
	if out.DataBits < 5 || out.DataBits > 8 {
		return out, fmt.Errorf("data_bits %d inválido (5..8)", out.DataBits)
	}
	if out.Parity == "" {
		out.Parity = "none"
	}
	switch out.Parity {
	case "none", "odd", "even", "mark", "space":
	default:
		return out, fmt.Errorf("parity %q inválida (none|odd|even|mark|space)", out.Parity)
	}
	if out.StopBits == "" {
		out.StopBits = "1"
	}
	switch out.StopBits {
	case "1", "1.5", "2":
	default:
		return out, fmt.Errorf("stop_bits %q inválido (1|1.5|2)", out.StopBits)
	}
	if out.FlowControl == "" {
		out.FlowControl = "none"
	}
	switch out.FlowControl {
	case "none", "xonxoff", "rtscts":
	default:
		return out, fmt.Errorf("flow_control %q inválido (none|xonxoff|rtscts)", out.FlowControl)
	}
	return out, nil
}

// EnsureDir crea el directorio del archivo indicado.
func EnsureDir(file string) error {
	dir := filepath.Dir(file)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

func boolPtr(v bool) *bool { return &v }
