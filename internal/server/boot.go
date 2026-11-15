package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/fiscal"
	"spoolerfiscal/internal/frame"
	pb "spoolerfiscal/proto/spooler/v1"
)

// Runtime agrupa las piezas del servidor ya cableadas.
type Runtime struct {
	cfg     config.Server
	log     *slog.Logger
	store   *Store
	spooler *Spooler
	service *Service
	grpc    *grpc.Server
	lis     net.Listener
	httpAPI *HTTPAPI
	httpSrv *http.Server
	httpLis net.Listener
	once    sync.Once
}

// NewRuntime abre la base, construye la impresora y prepara el listener gRPC.
func NewRuntime(cfg config.Server, log *slog.Logger) (*Runtime, error) {
	store, err := OpenStore(cfg.Database.Path)
	if err != nil {
		return nil, err
	}

	printer, err := buildPrinter(cfg, log)
	if err != nil {
		_ = store.Close()
		return nil, err
	}

	spooler := NewSpooler(printer, store, SpoolerConfig{
		MaxPending:            cfg.Queue.MaxPending,
		DefaultTimeout:        ms(cfg.Queue.DefaultTimeoutMS),
		MaxTimeout:            ms(cfg.Queue.MaxTimeoutMS),
		ProbeTimeout:          ms(cfg.Queue.ProbeTimeoutMS),
		OpenRetry:             ms(cfg.Printer.OpenRetryMS),
		DocumentLockTimeout:   ms(cfg.Queue.DocumentLockTimeoutMS),
		CorrelativeMaxAge:     ms(cfg.Queue.CorrelativeSyncMaxAgeMS),
		AutoCloseOpenDocument: cfg.Queue.AutoCloseOpenDocument == nil || *cfg.Queue.AutoCloseOpenDocument,
		LogCommands:           cfg.Database.LogCommands,
		CommandTerminator:     cfg.Printer.CommandTerminator,
		Guard: GuardConfig{
			BlockOnPaperOut:   cfg.Guard.BlockOnPaperOut,
			BlockOnMemoryFull: cfg.Guard.BlockOnMemoryFull,
			BlockOnError:      cfg.Guard.BlockOnError,
			BlockWhenOffline:  cfg.Guard.BlockWhenOffline,
		},
	}, log)

	service := NewService(ServiceConfig{
		Token:           cfg.Token,
		AllowedStations: cfg.AllowedStations,
		Version:         Version,
		Driver:          printer.Driver(),
		SessionTTL:      time.Duration(cfg.SessionTTLMin) * time.Minute,
		MaxCommandBytes: 64 * 1024,
	}, spooler, store, log)

	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(AuthUnaryInterceptor(cfg.Token)),
		grpc.MaxRecvMsgSize(4 * 1024 * 1024),
		grpc.MaxSendMsgSize(4 * 1024 * 1024),
	}
	if cfg.TLS.Enabled {
		creds, err := serverCredentials(cfg.TLS)
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		opts = append(opts, grpc.Creds(creds))
	}

	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("escuchar en %s: %w", cfg.Listen, err)
	}

	srv := grpc.NewServer(opts...)
	pb.RegisterFiscalSpoolerServer(srv, service)

	var httpAPI *HTTPAPI
	var httpSrv *http.Server
	var httpLis net.Listener
	if cfg.HTTP.Enabled {
		httpAPI = NewHTTPAPI(HTTPConfig{
			Token:             cfg.Token,
			AllowedStations:   cfg.AllowedStations,
			Version:           Version,
			Driver:            printer.Driver(),
			MaxBodyBytes:      cfg.HTTP.MaxBodyBytes,
			MaxLines:          500,
			MaxLineBytes:      1024,
			DefaultTimeout:    ms(cfg.Queue.DefaultTimeoutMS),
			MaxTimeout:        ms(cfg.Queue.MaxTimeoutMS),
			ProbeTimeout:      ms(cfg.Queue.ProbeTimeoutMS),
			JobResumeInterval: time.Duration(cfg.HTTP.JobResumeIntervalSeconds) * time.Second,
		}, spooler, store, log)
		httpLis, err = net.Listen("tcp", cfg.HTTP.Listen)
		if err != nil {
			_ = lis.Close()
			_ = store.Close()
			return nil, fmt.Errorf("escuchar API REST en %s: %w", cfg.HTTP.Listen, err)
		}
		httpSrv = &http.Server{
			Handler:           httpAPI.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       ms(cfg.HTTP.ReadTimeoutMS),
			WriteTimeout:      ms(cfg.HTTP.WriteTimeoutMS),
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    1 << 16,
		}
		if cfg.TLS.Enabled {
			tlsCfg, err := httpTLSConfig(cfg.TLS)
			if err != nil {
				_ = httpLis.Close()
				_ = lis.Close()
				_ = store.Close()
				return nil, err
			}
			// La REST autentica con token; no exige certificado de cliente.
			httpSrv.TLSConfig = tlsCfg
		}
	}

	return &Runtime{
		cfg: cfg, log: log, store: store, spooler: spooler,
		service: service, grpc: srv, lis: lis,
		httpAPI: httpAPI, httpSrv: httpSrv, httpLis: httpLis,
	}, nil
}

// Version es la versión del binario servidor.
const Version = "1.0.0"

// Addr devuelve la dirección real de escucha gRPC (útil con puerto 0).
func (r *Runtime) Addr() string { return r.lis.Addr().String() }

// HTTPAddr devuelve la dirección real de la API REST, o "" si está apagada.
func (r *Runtime) HTTPAddr() string {
	if r.httpLis == nil {
		return ""
	}
	return r.httpLis.Addr().String()
}

// Run arranca la cola y sirve gRPC hasta que el contexto se cancele.
func (r *Runtime) Run(ctx context.Context) error {
	if err := r.spooler.Start(ctx); err != nil {
		return err
	}

	if r.cfg.Database.RetentionDays > 0 {
		cut := time.Now().AddDate(0, 0, -r.cfg.Database.RetentionDays)
		if n, err := r.store.PruneCommands(ctx, cut); err != nil {
			r.log.Warn("purgar bitácora", "error", err)
		} else if n > 0 {
			r.log.Info("bitácora purgada", "filas", n, "retencion_dias", r.cfg.Database.RetentionDays)
		}
		if n, err := r.store.PruneJobs(ctx, cut); err != nil {
			r.log.Warn("purgar trabajos", "error", err)
		} else if n > 0 {
			r.log.Info("trabajos terminados purgados", "filas", n)
		}
	}

	// Trabajos interrumpidos por un corte: los que no llegaron a papel vuelven
	// a la cola; los parciales se abortan con evidencia (nunca se reimprime a
	// mitad de factura). Si la impresora quedó con documento abierto, se cura.
	if resumed, aborted, err := r.store.RecoverInterruptedJobs(ctx); err != nil {
		r.log.Warn("recuperar trabajos interrumpidos", "error", err)
	} else {
		if resumed > 0 {
			r.log.Info("trabajos devueltos a la cola tras reinicio", "trabajos", resumed)
		}
		if aborted > 0 {
			r.log.Warn("trabajos abortados por reinicio (parcialmente impresos)", "trabajos", aborted)
			if _, err := r.spooler.RecoverOpenDocument(ctx, ""); err != nil {
				r.log.Warn("recuperación de documento abierto al arrancar", "error", err)
			}
		}
	}

	go r.sweepLoop(ctx)
	go r.metricsLoop(ctx)
	if r.cfg.Queue.StatusProbeIntervalSeconds > 0 {
		go r.probeLoop(ctx)
	}
	if r.httpAPI != nil {
		go r.httpAPI.Dispatcher(ctx)
	}

	if r.cfg.HTTP.Enabled && r.cfg.HTTP.IdempotencyRetentionD > 0 {
		cut := time.Now().AddDate(0, 0, -r.cfg.HTTP.IdempotencyRetentionD)
		if n, err := r.store.PruneIdempotency(cut); err != nil {
			r.log.Warn("purgar claves de idempotencia", "error", err)
		} else if n > 0 {
			r.log.Info("claves de idempotencia purgadas", "filas", n)
		}
	}

	errCh := make(chan error, 2)
	go func() { errCh <- r.grpc.Serve(r.lis) }()
	if r.httpSrv != nil {
		go func() {
			var err error
			if r.httpSrv.TLSConfig != nil {
				err = r.httpSrv.ServeTLS(r.httpLis, "", "")
			} else {
				err = r.httpSrv.Serve(r.httpLis)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	snap := r.spooler.Snapshot()
	r.log.Info("spooler fiscal escuchando", "addr", r.lis.Addr().String(),
		"tls", r.cfg.TLS.Enabled, "driver", r.cfg.Printer.Driver,
		"historico_documentos", snap.History.Count,
		"ultima_factura", snap.History.LastNumbers[fiscal.DocInvoice])
	if r.httpSrv != nil {
		r.log.Info("api REST escuchando", "addr", r.httpLis.Addr().String(), "tls", r.cfg.TLS.Enabled, "docs", "/docs/API.md")
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	r.log.Info("deteniendo spooler fiscal")
	if r.httpSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.httpSrv.Shutdown(shutdownCtx); err != nil {
			r.log.Warn("cierre de la API REST", "error", err)
		}
		cancel()
	}
	done := make(chan struct{})
	go func() {
		r.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		r.log.Warn("cierre forzado de gRPC")
		r.grpc.Stop()
	}
	r.spooler.Stop()
	return nil
}

// Close libera base e impresora (idempotente).
func (r *Runtime) Close() error {
	var err error
	r.once.Do(func() {
		if r.httpSrv != nil {
			_ = r.httpSrv.Close()
		}
		if r.httpLis != nil {
			_ = r.httpLis.Close()
		}
		if r.lis != nil {
			_ = r.lis.Close()
		}
		r.spooler.Stop()
		err = r.store.Close()
	})
	return err
}

func (r *Runtime) sweepLoop(ctx context.Context) {
	ttl := time.Duration(r.cfg.SessionTTLMin) * time.Minute
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.service.Sweep(ttl)
		}
	}
}

// probeLoop mantiene el estado del guardián fresco: consulta la impresora
// periódicamente cuando la cola está vacía, de modo que papel agotado, memoria
// llena o documento colgado se detecten sin esperar una emisión fallida.
func (r *Runtime) probeLoop(ctx context.Context) {
	interval := time.Duration(r.cfg.Queue.StatusProbeIntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap := r.spooler.Snapshot()
			if snap.QueueDepth > 0 || snap.DocumentOpen {
				continue // no competir con emisiones en curso
			}
			probeCtx, cancel := context.WithTimeout(ctx, ms(r.cfg.Queue.ProbeTimeoutMS))
			_, err := r.spooler.ProbeStatus(probeCtx)
			cancel()
			if err != nil {
				r.log.Debug("sonda periódica falló", "error", err)
			}
		}
	}
}

func (r *Runtime) metricsLoop(ctx context.Context) {
	interval := time.Duration(r.cfg.Metrics.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := r.spooler.Snapshot()
			r.log.Info("métricas",
				"documentos", s.Counters.Documents,
				"comandos", s.Counters.Commands,
				"errores", s.Counters.Failures,
				"duplicados", s.Counters.Duplicates,
				"latencia_prom_ms", fmt.Sprintf("%.1f", s.AvgLatency),
				"latencia_max_ms", s.Counters.LatencyMaxMS,
				"cola", s.QueueDepth,
				"online", s.Status.Online,
				"bloqueado", s.Blocked,
			)
		}
	}
}

func buildPrinter(cfg config.Server, log *slog.Logger) (fiscal.Printer, error) {
	pc := cfg.Printer
	params, err := config.ParsePort(pc.Port)
	if err != nil {
		return nil, err
	}
	if params.FlowControl == "rtscts" && pc.Transport == "serial" {
		log.Warn("flow_control=rtscts: go.bug.st/serial no configura el handshake del driver; " +
			"actívelo en Administrador de dispositivos > Puerto > Configuración > Avanzado")
	}

	var factory func() (frame.Stream, error)
	label := ""
	switch pc.Transport {
	case "tcp":
		addr := pc.TCPAddress
		timeout := ms(pc.TCPDialTimeoutMS)
		factory = func() (frame.Stream, error) { return fiscal.OpenTCP(addr, timeout) }
		label = "tcp://" + addr
	default:
		serialCfg := fiscal.SerialConfig{
			Name: pc.Port.Name, BaudRate: params.BaudRate, DataBits: params.DataBits,
			Parity: params.Parity, StopBits: params.StopBits, FlowControl: params.FlowControl,
		}
		factory = func() (frame.Stream, error) { return fiscal.OpenSerial(serialCfg) }
		label = fmt.Sprintf("%s @ %d %d%s%s", serialCfg.Name, serialCfg.BaudRate, serialCfg.DataBits,
			parityLetter(serialCfg.Parity), serialCfg.StopBits)
	}

	printer, err := fiscal.New(cfg.Printer.Driver, fiscal.Options{
		NewTransport:     factory,
		ReadGap:          ms(pc.ReadGapMS),
		ResponseTimeout:  ms(pc.ResponseTimeoutMS),
		MaxResponseBytes: pc.MaxResponseBytes,
		WriteSettle:      ms(pc.WriteSettleMS),
		StatusProbe:      pc.StatusProbe,
		CommandEnding:    pc.CommandTerminator,
		PortLabel:        label,
		Logger:           log,
	})
	if err != nil {
		return nil, err
	}
	log.Info("impresora fiscal configurada", "driver", cfg.Printer.Driver, "transporte", pc.Transport, "destino", label)
	return printer, nil
}

func serverCredentials(cfg config.TLS) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("cargar certificado del servidor: %w", err)
	}
	tc := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("leer CA de clientes: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA de clientes inválida: %s", cfg.CAFile)
		}
		tc.ClientCAs = pool
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(tc), nil
}

func httpTLSConfig(cfg config.TLS) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("cargar certificado para la API REST: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

func ms(v int) time.Duration { return time.Duration(v) * time.Millisecond }

func parityLetter(p string) string {
	switch p {
	case "odd":
		return "O"
	case "even":
		return "E"
	case "mark":
		return "M"
	case "space":
		return "S"
	default:
		return "N"
	}
}
