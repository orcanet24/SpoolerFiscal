// Package agent contiene el cliente HTTP del servicio de estación: mantiene el
// puente entre el COM virtual que ve Profit Plus y la API del spooler central.
// Reenvía los bytes fiscales sin interpretarlos y devuelve la respuesta real.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"spoolerfiscal/internal/config"
)

// Version del binario agente.
const Version = "1.0.0"

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

// commandResponse es el contrato de POST /v1/commands del spooler.
type commandResponse struct {
	Station        string `json:"station"`
	Session        string `json:"session"`
	Response       string `json:"response"`
	ResponseBase64 string `json:"response_base64"`
	Sequence       int64  `json:"sequence"`
	QueueWaitMS    int64  `json:"queue_wait_ms"`
	LatencyMS      int64  `json:"latency_ms"`
	Status         struct {
		Blocked       bool   `json:"blocked"`
		BlockedReason string `json:"blocked_reason"`
	} `json:"status"`
	Document *struct {
		DocType   string `json:"doc_type"`
		DocNumber int64  `json:"doc_number"`
		Sequence  int64  `json:"sequence"`
		Duplicate bool   `json:"duplicate"`
	} `json:"document"`
}

// Agent es el cliente HTTP del spooler en una estación.
type Agent struct {
	cfg     config.Agent
	log     *slog.Logger
	jobs    chan *request
	seq     atomic.Uint64
	frames  atomic.Uint64
	errs    atomic.Uint64
	retries atomic.Uint64

	client  *http.Client
	baseURL string
	online  atomic.Bool

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
	timeout := time.Duration(cfg.Server.RequestTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	if cfg.Server.TLS.Enabled {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.Server.TLS.CAFile != "" {
			pem, err := os.ReadFile(cfg.Server.TLS.CAFile)
			if err != nil {
				log.Error("no se pudo leer la CA del spooler", "error", err)
			} else {
				pool := x509.NewCertPool()
				if !pool.AppendCertsFromPEM(pem) {
					log.Error("CA del spooler inválida", "archivo", cfg.Server.TLS.CAFile)
				} else {
					tlsCfg.RootCAs = pool
				}
			}
		}
		// Certificado de cliente para mTLS: el servidor lo exige si tiene
		// tls.ca_file configurado.
		if cfg.Server.TLS.CertFile != "" && cfg.Server.TLS.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
			if err != nil {
				log.Error("certificado de cliente TLS inválido", "error", err,
					"cert", cfg.Server.TLS.CertFile, "key", cfg.Server.TLS.KeyFile)
			} else {
				tlsCfg.Certificates = []tls.Certificate{cert}
			}
		}
		client.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	return &Agent{
		cfg: cfg, log: log, jobs: make(chan *request, size),
		client: client, baseURL: normalizeBase(cfg.Server.Address),
	}
}

// Start lanza el worker que serializa los reenvíos y la sonda de salud.
func (a *Agent) Start(ctx context.Context) error {
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.wg.Add(2)
	go a.worker()
	go a.healthLoop()
	a.log.Info("agente iniciado", "estacion", a.cfg.StationID, "servidor", a.baseURL,
		"com_virtual", a.cfg.VirtualCOM.Name, "cola_local", cap(a.jobs))
	return nil
}

// Close detiene el worker y la sonda.
func (a *Agent) Close() {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
}

// Stats devuelve contadores para el log periódico.
func (a *Agent) Stats() (frames, errs, retries uint64) {
	return a.frames.Load(), a.errs.Load(), a.retries.Load()
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

// dispatch reintenta mientras el servidor no esté disponible. Cada intento
// viaja con la misma Idempotency-Key (la referencia del comando): si la
// petición anterior llegó y el spooler la ejecutó, el reintento recibe la
// misma respuesta y no se imprime dos veces.
func (a *Agent) dispatch(ctx context.Context, req *request) ([]byte, error) {
	backoff := 100 * time.Millisecond
	maxBackoff := time.Duration(a.cfg.Bridge.MaxSendBackoffMS) * time.Millisecond
	if maxBackoff <= 0 {
		maxBackoff = 2 * time.Second
	}
	var lastErr error

	for {
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

		remaining := time.Until(req.deadline)
		if remaining <= 0 {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, remaining)
		status, resp, err := a.send(callCtx, req)
		cancel()

		if err == nil {
			a.frames.Add(1)
			if resp.Status.Blocked {
				a.log.Warn("el spooler reporta impresora bloqueada",
					"motivo", resp.Status.BlockedReason, "ref", req.ref)
			}
			if resp.Document != nil {
				a.log.Info("documento registrado por el spooler", "tipo", resp.Document.DocType,
					"numero", resp.Document.DocNumber, "secuencia", resp.Document.Sequence,
					"duplicado", resp.Document.Duplicate)
			}
			return decodeResponse(resp)
		}

		lastErr = err
		if status != 0 && !retryableStatus(status) {
			a.errs.Add(1)
			a.log.Error("el spooler rechazó el comando; no se reintenta",
				"ref", req.ref, "http", status, "error", err)
			return nil, err
		}
		a.retries.Add(1)
		a.sleep(ctx, &backoff, maxBackoff, req.deadline)
	}
}

// send ejecuta un POST /v1/commands. Devuelve el código HTTP (0 si no hubo
// respuesta) y el error de transporte o de estado.
func (a *Agent) send(ctx context.Context, req *request) (int, *commandResponse, error) {
	timeoutMS := a.cfg.Server.RequestTimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = 30000
	}
	body, err := json.Marshal(map[string]any{
		"station":        a.cfg.StationID,
		"command_base64": base64.StdEncoding.EncodeToString(req.frame),
		"client_ref":     req.ref,
		"timeout_ms":     timeoutMS,
	})
	if err != nil {
		return 0, nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/commands", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.cfg.Server.Token)
	// La referencia del comando es la clave de idempotencia: un reintento por
	// red no puede duplicar la emisión.
	httpReq.Header.Set("Idempotency-Key", req.ref)

	res, err := a.client.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return res.StatusCode, nil, err
	}
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, nil, fmt.Errorf("%s", errorMessage(data, res.StatusCode))
	}
	var out commandResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return res.StatusCode, nil, fmt.Errorf("respuesta del spooler inválida: %w", err)
	}
	return res.StatusCode, &out, nil
}

// healthLoop registra el cambio de conectividad contra la API del spooler.
func (a *Agent) healthLoop() {
	defer a.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			ok := a.health()
			if ok != a.online.Load() {
				a.online.Store(ok)
				if ok {
					a.log.Info("conectado al spooler", "servidor", a.baseURL)
				} else {
					a.log.Warn("spooler no disponible; se reintentará", "servidor", a.baseURL)
				}
			}
		}
	}
}

func (a *Agent) health() bool {
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/health", nil)
	if err != nil {
		return false
	}
	res, err := a.client.Do(req)
	if err != nil {
		return false
	}
	_ = res.Body.Close()
	return res.StatusCode == http.StatusOK
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

func decodeResponse(resp *commandResponse) ([]byte, error) {
	if resp.ResponseBase64 != "" {
		out, err := base64.StdEncoding.DecodeString(resp.ResponseBase64)
		if err != nil {
			return nil, fmt.Errorf("respuesta base64 inválida: %w", err)
		}
		return out, nil
	}
	return []byte(resp.Response), nil
}

func normalizeBase(addr string) string {
	a := strings.TrimSpace(addr)
	if a == "" {
		return ""
	}
	if !strings.Contains(a, "://") {
		a = "http://" + a
	}
	return strings.TrimRight(a, "/")
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable:
		return true
	}
	return false
}

// errorMessage extrae el mensaje de un errorEnvelope; si no lo reconoce,
// devuelve el código y una descripción genérica.
func errorMessage(data []byte, status int) string {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err == nil && env.Error.Message != "" {
		if env.Error.Code != "" {
			return fmt.Sprintf("HTTP %d (%s): %s", status, env.Error.Code, env.Error.Message)
		}
		return fmt.Sprintf("HTTP %d: %s", status, env.Error.Message)
	}
	if len(data) == 0 {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, strings.TrimSpace(string(data)))
}
