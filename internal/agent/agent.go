// Package agent contiene el cliente gRPC del servicio de estación: mantiene la
// sesión con el spooler, reintenta mientras el servidor no esté disponible y
// reenvía bytes fiscales sin interpretarlos.
package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"spoolerfiscal/internal/config"
	pb "spoolerfiscal/proto/spooler/v1"
)

// Version del binario agente.
const Version = "1.0.0"

// serverProtocolVersion es la versión del contrato gRPC que entiende este agente.
const serverProtocolVersion uint32 = 1

// Errores del agente.
var (
	// ErrBusy indica que la cola local está llena.
	ErrBusy = errors.New("agente: cola local llena, impresora ocupada")
	// ErrUnavailable indica que el spooler no respondió a tiempo.
	ErrUnavailable = errors.New("agente: spooler no disponible")
)

type request struct {
	frame    []byte
	ref      string
	deadline time.Time
	res      chan result
}

type result struct {
	resp []byte
	err  error
}

// Agent es el cliente del spooler en una estación.
type Agent struct {
	cfg     config.Agent
	log     *slog.Logger
	jobs    chan *request
	seq     atomic.Uint64
	frames  atomic.Uint64
	errs    atomic.Uint64
	reconns atomic.Uint64
	retries atomic.Uint64

	mu        sync.Mutex
	conn      *grpc.ClientConn
	client    pb.FiscalSpoolerClient
	sessionID string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New construye el agente.
func New(cfg config.Agent, log *slog.Logger) *Agent {
	if log == nil {
		log = slog.Default()
	}
	size := cfg.Bridge.LocalQueueSize
	if size <= 0 {
		size = 16
	}
	return &Agent{cfg: cfg, log: log, jobs: make(chan *request, size)}
}

// Start lanza el worker que serializa los reenvíos.
func (a *Agent) Start(ctx context.Context) error {
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.wg.Add(1)
	go a.worker()
	a.log.Info("agente iniciado", "estacion", a.cfg.StationID, "servidor", a.cfg.Server.Address,
		"com_virtual", a.cfg.VirtualCOM.Name, "cola_local", cap(a.jobs))
	return nil
}

// Close cierra la conexión y espera al worker.
func (a *Agent) Close() {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	a.mu.Lock()
	conn := a.conn
	a.conn, a.client, a.sessionID = nil, nil, ""
	a.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// Stats devuelve contadores para el log periódico.
func (a *Agent) Stats() (frames, errs, reconnects, retries uint64) {
	return a.frames.Load(), a.errs.Load(), a.reconns.Load(), a.retries.Load()
}

// Forward entrega una trama al spooler y devuelve la respuesta cruda.
func (a *Agent) Forward(ctx context.Context, frame []byte) ([]byte, error) {
	if len(frame) == 0 {
		return nil, nil
	}
	timeout := time.Duration(a.cfg.Server.RequestTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	req := &request{
		frame:    frame,
		ref:      fmt.Sprintf("%s/%d", a.cfg.StationID, a.seq.Add(1)),
		deadline: time.Now().Add(timeout),
		res:      make(chan result, 1),
	}
	select {
	case a.jobs <- req:
	default:
		a.errs.Add(1)
		return nil, ErrBusy
	}
	select {
	case r := <-req.res:
		return r.resp, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *Agent) worker() {
	defer a.wg.Done()
	for {
		select {
		case <-a.ctx.Done():
			return
		case req := <-a.jobs:
			resp, err := a.dispatch(a.ctx, req)
			req.res <- result{resp: resp, err: err}
		}
	}
}

// dispatch espera al servidor con backoff mientras no haya conexión. Una vez
// que el comando salió hacia el servidor NO se reenvía: un reenvío podría
// imprimir dos veces la misma factura.
func (a *Agent) dispatch(ctx context.Context, req *request) ([]byte, error) {
	backoff := 100 * time.Millisecond
	maxBackoff := time.Duration(a.cfg.Bridge.MaxSendBackoffMS) * time.Millisecond
	if maxBackoff <= 0 {
		maxBackoff = 2 * time.Second
	}
	var lastErr error

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !time.Now().Before(req.deadline) {
			a.errs.Add(1)
			if lastErr == nil {
				lastErr = ErrUnavailable
			}
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, lastErr)
		}

		client, err := a.clientFor(ctx)
		if err != nil {
			lastErr = err
			a.sleep(ctx, &backoff, maxBackoff, req.deadline)
			continue
		}

		session, err := a.ensureSession(ctx, client)
		if err != nil {
			lastErr = err
			a.dropConnection()
			a.sleep(ctx, &backoff, maxBackoff, req.deadline)
			continue
		}

		remaining := time.Until(req.deadline)
		if remaining <= 0 {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, remaining)
		resp, err := client.SendCommand(a.auth(callCtx), &pb.CommandRequest{
			SessionId: session,
			Command:   req.frame,
			TimeoutMs: uint32(remaining.Milliseconds()),
			ClientRef: req.ref,
		})
		cancel()
		if err == nil {
			a.frames.Add(1)
			if resp.Status != nil && resp.Status.Blocked {
				a.log.Warn("el spooler reporta impresora bloqueada",
					"motivo", resp.Status.BlockedReason, "ref", req.ref)
			}
			if resp.Document != nil {
				a.log.Info("documento registrado por el spooler", "tipo", resp.Document.DocType,
					"numero", resp.Document.DocNumber, "secuencia", resp.Document.Sequence,
					"duplicado", resp.Document.Duplicate)
			}
			return resp.Response, nil
		}

		lastErr = err
		// Sesión inexistente: el servidor rechazó antes de encolar, es seguro
		// volver a abrir sesión y reenviar.
		if status.Code(err) == codes.NotFound {
			a.retries.Add(1)
			a.resetSession()
			a.log.Warn("sesión inválida, reabriendo", "ref", req.ref, "error", err)
			continue
		}

		a.errs.Add(1)
		a.log.Error("fallo al reenviar el comando; no se reintenta para evitar doble emisión",
			"ref", req.ref, "codigo", status.Code(err).String(), "error", err)
		if status.Code(err) == codes.Unavailable {
			a.dropConnection()
		}
		return nil, err
	}
}

func (a *Agent) sleep(ctx context.Context, backoff *time.Duration, maxBackoff time.Duration, deadline time.Time) {
	d := *backoff
	if remain := time.Until(deadline); remain < d {
		d = remain
	}
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	*backoff *= 2
	if *backoff > maxBackoff {
		*backoff = maxBackoff
	}
}

func (a *Agent) auth(ctx context.Context) context.Context {
	if a.cfg.Server.Token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.cfg.Server.Token)
}

func (a *Agent) clientFor(ctx context.Context) (pb.FiscalSpoolerClient, error) {
	a.mu.Lock()
	if a.client != nil {
		c := a.client
		a.mu.Unlock()
		return c, nil
	}
	a.mu.Unlock()

	opts, err := a.dialOptions()
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(a.cfg.Server.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("crear cliente gRPC: %w", err)
	}
	client := pb.NewFiscalSpoolerClient(conn)

	dialTimeout := time.Duration(a.cfg.Server.DialTimeoutMS) * time.Millisecond
	if dialTimeout <= 0 {
		dialTimeout = 3 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	ping, err := client.Ping(a.auth(pingCtx), &pb.PingRequest{ClientVersion: Version})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping al spooler: %w", err)
	}
	if ping.ProtocolVersion != serverProtocolVersion {
		_ = conn.Close()
		return nil, fmt.Errorf("versión de protocolo incompatible: servidor %d, agente %d",
			ping.ProtocolVersion, serverProtocolVersion)
	}

	a.mu.Lock()
	a.conn = conn
	a.client = client
	a.mu.Unlock()
	a.reconns.Add(1)
	a.log.Info("conectado al spooler", "servidor", a.cfg.Server.Address,
		"version_servidor", ping.ServerVersion, "driver", ping.PrinterDriver)
	return client, nil
}

func (a *Agent) ensureSession(ctx context.Context, client pb.FiscalSpoolerClient) (string, error) {
	a.mu.Lock()
	if a.sessionID != "" {
		id := a.sessionID
		a.mu.Unlock()
		return id, nil
	}
	a.mu.Unlock()

	timeout := time.Duration(a.cfg.Server.DialTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := client.OpenSession(a.auth(callCtx), &pb.OpenSessionRequest{
		StationId:     a.cfg.StationID,
		ClientVersion: Version,
	})
	if err != nil {
		return "", fmt.Errorf("abrir sesión: %w", err)
	}

	a.mu.Lock()
	a.sessionID = resp.SessionId
	a.mu.Unlock()
	if resp.Printer != nil {
		a.log.Info("sesión abierta", "sesion", resp.SessionId,
			"impresora", resp.Printer.Model, "serial", resp.Printer.Serial,
			"rif", resp.Printer.FiscalRif,
			"ultima_factura", resp.Printer.LastInvoiceNumber)
	} else {
		a.log.Info("sesión abierta", "sesion", resp.SessionId)
	}
	return resp.SessionId, nil
}

func (a *Agent) resetSession() {
	a.mu.Lock()
	a.sessionID = ""
	a.mu.Unlock()
}

func (a *Agent) dropConnection() {
	a.mu.Lock()
	conn := a.conn
	a.conn, a.client, a.sessionID = nil, nil, ""
	a.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (a *Agent) dialOptions() ([]grpc.DialOption, error) {
	tlsCfg := a.cfg.Server.TLS
	if !tlsCfg.Enabled {
		return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if tlsCfg.CAFile != "" {
		pem, err := os.ReadFile(tlsCfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("leer CA del spooler: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA inválida: %s", tlsCfg.CAFile)
		}
		cfg.RootCAs = pool
	}
	if tlsCfg.CertFile != "" && tlsCfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(tlsCfg.CertFile, tlsCfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("cargar certificado del agente: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(cfg))}, nil
}
