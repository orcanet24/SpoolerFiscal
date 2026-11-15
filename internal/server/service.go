package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"spoolerfiscal/internal/fiscal"
	pb "spoolerfiscal/proto/spooler/v1"
)

// ProtocolVersion es la versión del contrato gRPC. El agente la valida en Ping.
const ProtocolVersion uint32 = 1

// ServiceConfig configura la fachada gRPC.
type ServiceConfig struct {
	Token           string
	AllowedStations []string
	Version         string
	Driver          string
	SessionTTL      time.Duration
	MaxCommandBytes int
}

type session struct {
	id       string
	station  string
	opened   time.Time
	lastSeen time.Time
	commands uint64
}

// Service implementa spooler.v1.FiscalSpooler.
type Service struct {
	pb.UnimplementedFiscalSpoolerServer

	cfg     ServiceConfig
	spooler *Spooler
	store   *Store
	log     *slog.Logger

	mu        sync.Mutex
	sessions  map[string]*session
	byStation map[string]string
	allowed   map[string]struct{}
}

// NewService construye la fachada gRPC.
func NewService(cfg ServiceConfig, spooler *Spooler, store *Store, log *slog.Logger) *Service {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 2 * time.Hour
	}
	if cfg.MaxCommandBytes <= 0 {
		cfg.MaxCommandBytes = 64 * 1024
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedStations))
	for _, s := range cfg.AllowedStations {
		allowed[strings.TrimSpace(s)] = struct{}{}
	}
	return &Service{
		cfg: cfg, spooler: spooler, store: store, log: log,
		sessions: map[string]*session{}, byStation: map[string]string{}, allowed: allowed,
	}
}

// Ping valida conectividad, token y versión del protocolo.
func (s *Service) Ping(ctx context.Context, req *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{
		ServerVersion:   s.cfg.Version,
		ProtocolVersion: ProtocolVersion,
		ServerTimeUnix:  time.Now().Unix(),
		PrinterDriver:   s.cfg.Driver,
	}, nil
}

// OpenSession registra la estación y reserva su sesión.
func (s *Service) OpenSession(ctx context.Context, req *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	station := strings.TrimSpace(req.GetStationId())
	if station == "" {
		return nil, status.Error(codes.InvalidArgument, "station_id es obligatorio")
	}
	if len(station) > 64 {
		return nil, status.Error(codes.InvalidArgument, "station_id demasiado largo (máx. 64)")
	}
	if len(s.allowed) > 0 {
		if _, ok := s.allowed[station]; !ok {
			s.log.Warn("estación rechazada por lista blanca", "estacion", station)
			return nil, status.Errorf(codes.PermissionDenied, "estación %q no autorizada", station)
		}
	}
	id, err := newSessionID()
	if err != nil {
		return nil, status.Error(codes.Internal, "no se pudo generar la sesión")
	}
	now := time.Now()
	sess := &session{id: id, station: station, opened: now, lastSeen: now}

	s.mu.Lock()
	if old, ok := s.byStation[station]; ok {
		delete(s.sessions, old)
	}
	s.sessions[id] = sess
	s.byStation[station] = id
	s.mu.Unlock()

	s.log.Info("sesión abierta", "estacion", station, "sesion", id, "cliente", req.GetClientVersion())

	// Conciliación de correlativo en segundo plano: cuando esta estación envíe
	// su primera factura, el número real de la impresora ya está cargado.
	s.spooler.TriggerCorrelativeSync()

	snap := s.spooler.Snapshot()
	return &pb.OpenSessionResponse{
		SessionId:    id,
		Printer:      infoToProto(snap.Info),
		Status:       statusToProto(snap.Status, snap.QueueDepth, snap.Blocked, snap.DocumentOpen, snap.DocumentOwner),
		LastSequence: snap.History.LastSequence,
	}, nil
}

// CloseSession libera la sesión de la estación.
func (s *Service) CloseSession(ctx context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	s.mu.Lock()
	sess, ok := s.sessions[req.GetSessionId()]
	if ok {
		delete(s.sessions, sess.id)
		if s.byStation[sess.station] == sess.id {
			delete(s.byStation, sess.station)
		}
	}
	s.mu.Unlock()
	if !ok {
		return &pb.CloseSessionResponse{Closed: false}, nil
	}
	s.log.Info("sesión cerrada", "estacion", sess.station, "sesion", sess.id, "comandos", sess.commands)
	return &pb.CloseSessionResponse{Closed: true, CommandsProcessed: sess.commands}, nil
}

// SendCommand transporta bytes fiscales y devuelve la respuesta íntegra.
func (s *Service) SendCommand(ctx context.Context, req *pb.CommandRequest) (*pb.CommandResponse, error) {
	sess, err := s.session(req.GetSessionId())
	if err != nil {
		return nil, err
	}
	cmd := req.GetCommand()
	if len(cmd) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command vacío")
	}
	if len(cmd) > s.cfg.MaxCommandBytes {
		return nil, status.Errorf(codes.InvalidArgument, "command excede %d bytes", s.cfg.MaxCommandBytes)
	}

	res, err := s.spooler.Submit(ctx, SubmitRequest{
		Command:   cmd,
		Station:   sess.station,
		Session:   sess.id,
		ClientRef: req.GetClientRef(),
		Timeout:   time.Duration(req.GetTimeoutMs()) * time.Millisecond,
	})
	if err != nil {
		return nil, mapSpoolerError(err)
	}

	s.mu.Lock()
	sess.commands++
	s.mu.Unlock()

	snap := s.spooler.Snapshot()
	resp := &pb.CommandResponse{
		Response:    res.Response,
		Status:      statusToProto(res.Status, snap.QueueDepth, snap.Blocked, snap.DocumentOpen, snap.DocumentOwner),
		Sequence:    res.Sequence,
		QueueWaitMs: uint64(max64(res.QueueWait.Milliseconds(), 0)),
		LatencyMs:   uint64(max64(res.Latency.Milliseconds(), 0)),
	}
	if res.Document != nil {
		resp.Document = docToProto(res.Document)
	}
	return resp, nil
}

// GetStatus devuelve estado de impresora, cola y contadores.
func (s *Service) GetStatus(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	if _, err := s.session(req.GetSessionId()); err != nil {
		return nil, err
	}
	if req.GetProbe() {
		if st, err := s.spooler.ProbeStatus(ctx); err != nil {
			s.log.Warn("sonda de estado falló", "error", err)
		} else {
			s.log.Debug("estado consultado", "online", st.Online, "papel", st.PaperOut)
		}
	}
	snap := s.spooler.Snapshot()
	resp := &pb.StatusResponse{
		Status:             statusToProto(snap.Status, snap.QueueDepth, snap.Blocked, snap.DocumentOpen, snap.DocumentOwner),
		QueueDepth:         uint32(max64(int64(snap.QueueDepth), 0)),
		DocumentsIssued:    snap.Counters.Documents,
		CommandsTotal:      snap.Counters.Commands,
		CommandsFailed:     snap.Counters.Failures,
		AvgLatencyMs:       snap.AvgLatency,
		MaxLatencyMs:       float64(snap.Counters.LatencyMaxMS),
		DuplicatesDetected: snap.Counters.Duplicates,
		UptimeSeconds:      int64(snap.Uptime.Seconds()),
		LastSequence:       snap.LastSeq,
	}
	if snap.History.LastDocument != nil {
		d := snap.History.LastDocument
		resp.LastDocumentSummary = strings.TrimSpace(d.Type + " #" + itoa(d.Number) + " " + d.StationID)
	}
	return resp, nil
}

// GetPrinterInfo devuelve modelo, serial, firmware y memoria fiscal.
func (s *Service) GetPrinterInfo(ctx context.Context, req *pb.PrinterInfoRequest) (*pb.PrinterInfoResponse, error) {
	if _, err := s.session(req.GetSessionId()); err != nil {
		return nil, err
	}
	info, err := s.spooler.ProbeInfo(ctx)
	if err != nil && info.Serial == "" {
		return nil, status.Errorf(codes.Unavailable, "no se pudo consultar la impresora: %v", err)
	}
	return &pb.PrinterInfoResponse{Info: infoToProto(info)}, nil
}

// Sweep elimina sesiones inactivas y devuelve cuántas cerró.
func (s *Service) Sweep(ttl time.Duration) int {
	cut := time.Now().Add(-ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, sess := range s.sessions {
		if sess.lastSeen.Before(cut) {
			delete(s.sessions, id)
			if s.byStation[sess.station] == id {
				delete(s.byStation, sess.station)
			}
			n++
			s.log.Info("sesión expirada", "estacion", sess.station, "sesion", id)
		}
	}
	return n
}

func (s *Service) session(id string) (*session, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id es obligatorio")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, status.Error(codes.NotFound, "sesión no encontrada o expirada; vuelva a abrir sesión")
	}
	sess.lastSeen = time.Now()
	return sess, nil
}

func newSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func mapSpoolerError(err error) error {
	var blocked *BlockedError
	switch {
	case errors.Is(err, ErrQueueFull):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.As(err, &blocked):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrPrinterUnavailable), errors.Is(err, ErrPrinterTimeout), errors.Is(err, ErrClosed):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "tiempo de espera agotado en la cola")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "petición cancelada")
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func statusToProto(st fiscal.Status, queueDepth int, blocked string, docOpen bool, docOwner string) *pb.PrinterStatus {
	return &pb.PrinterStatus{
		Online:                 st.Online,
		StatusByte:             uint32(st.StatusByte),
		ErrorByte:              uint32(st.ErrorByte),
		StatusText:             st.StatusText,
		ErrorText:              st.ErrorText,
		PaperOut:               st.PaperOut,
		FiscalMemoryFull:       st.MemoryFull,
		FiscalMemoryAlmostFull: st.MemoryAlmostFull,
		DocumentOpen:           st.DocumentOpen || docOpen,
		DocumentOwner:          docOwner,
		Blocked:                blocked != "",
		BlockedReason:          blocked,
		LastError:              st.LastError,
		CheckedAtUnix:          st.Checked.Unix(),
		QueueDepth:             uint32(max64(int64(queueDepth), 0)),
		Raw:                    st.Raw,
	}
}

func infoToProto(info fiscal.Info) *pb.PrinterInfo {
	return &pb.PrinterInfo{
		Brand: info.Brand, Model: info.Model, Serial: info.Serial, Firmware: info.Firmware,
		FiscalRif: info.RIF, FiscalMemoryTotalMb: info.MemoryTotalMB,
		FiscalMemoryFreeMb: info.MemoryFreeMB, LastInvoiceNumber: info.LastInvoice,
		LastCreditNoteNumber: info.LastCredit, LastDebitNoteNumber: info.LastDebit,
		Port: info.Port,
	}
}

func docToProto(d *DocumentRecord) *pb.DocumentRecord {
	return &pb.DocumentRecord{
		Sequence: d.Sequence, DocType: d.Type, DocNumber: d.Number,
		StationId: d.StationID, SessionId: d.SessionID, IssuedAtUnix: d.IssuedAt.Unix(),
		RequestSha256: d.RequestSHA256, ResponseSha256: d.ResponseSHA256,
		ResponseText: d.ResponseText, Duplicate: d.Duplicate, ClientRef: d.ClientRef,
	}
}

// AuthUnaryInterceptor valida el token compartido en cada RPC.
func AuthUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !validToken(ctx, token) {
			return nil, status.Error(codes.Unauthenticated, "token de estación inválido")
		}
		return handler(ctx, req)
	}
}

func validToken(ctx context.Context, want string) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	got := ""
	if v := md.Get("authorization"); len(v) > 0 {
		got = strings.TrimSpace(v[0])
		if len(got) > 7 && strings.EqualFold(got[:7], "bearer ") {
			got = strings.TrimSpace(got[7:])
		}
	}
	if got == "" {
		if v := md.Get("x-spooler-token"); len(v) > 0 {
			got = strings.TrimSpace(v[0])
		}
	}
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
