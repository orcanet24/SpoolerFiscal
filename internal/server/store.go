package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"spoolerfiscal/internal/fiscal"
)

// DocumentRecord es un documento fiscal registrado por el spooler. La
// numeración la asigna la impresora; el spooler la audita y le agrega una
// secuencia global propia.
type DocumentRecord struct {
	Sequence       int64
	Type           string
	Number         int64
	StationID      string
	SessionID      string
	IssuedAt       time.Time
	StatusByte     byte
	ErrorByte      byte
	RequestSHA256  string
	ResponseSHA256 string
	ResponseText   string
	ClientRef      string
	Duplicate      bool
}

// CommandLog es una entrada de la bitácora de comandos ejecutados.
type CommandLog struct {
	At             time.Time
	StationID      string
	SessionID      string
	RequestSHA256  string
	ResponseSHA256 string
	OK             bool
	LatencyMS      int64
	Error          string
}

// History es el resumen persistido del correlativo.
type History struct {
	Count        int64
	LastSequence int64
	LastDocument *DocumentRecord
	LastNumbers  map[string]int64
}

// Store envuelve la base SQLite embebida (driver puro Go, sin cgo).
type Store struct {
	db   *sql.DB
	path string
}

// OpenStore abre (o crea) la base y aplica el esquema.
func OpenStore(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("crear directorio de base de datos: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abrir base %s: %w", path, err)
	}
	// SQLite serializa escrituras; una sola conexión evita bloqueos entre
	// la bitácora, el registro de documentos y las consultas de estado.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS documents (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			doc_type TEXT NOT NULL,
			doc_number INTEGER NOT NULL,
			station_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			issued_at TEXT NOT NULL,
			status_byte INTEGER NOT NULL DEFAULT 0,
			error_byte INTEGER NOT NULL DEFAULT 0,
			request_sha256 TEXT NOT NULL,
			response_sha256 TEXT NOT NULL,
			response_text TEXT NOT NULL,
			client_ref TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_documents_number
			ON documents(doc_type, doc_number) WHERE doc_number > 0`,
		`CREATE INDEX IF NOT EXISTS ix_documents_station ON documents(station_id)`,
		`CREATE TABLE IF NOT EXISTS command_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at TEXT NOT NULL,
			station_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			request_sha256 TEXT NOT NULL,
			response_sha256 TEXT NOT NULL,
			ok INTEGER NOT NULL,
			latency_ms INTEGER NOT NULL,
			error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS ix_command_log_at ON command_log(at)`,
		`CREATE TABLE IF NOT EXISTS printer_state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS idempotency_keys (
			key TEXT PRIMARY KEY,
			request_hash TEXT NOT NULL,
			status INTEGER NOT NULL,
			response BLOB NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS ix_idempotency_created ON idempotency_keys(created_at)`,
		// Correlativo esperado por tipo: la impresora es la fuente legal; esta
		// tabla se concilia con S1 (pre-vuelo) y con cada documento impreso.
		`CREATE TABLE IF NOT EXISTS correlative_state (
			doc_type TEXT PRIMARY KEY,
			last_number INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT 'db',
			updated_at TEXT NOT NULL
		)`,
		// Cola duradera de facturas REST. POS (Profit Plus) sigue interactivo:
		// sus comandos no se persisten. Sólo la vía web/ERP es reanudable.
		`CREATE TABLE IF NOT EXISTS print_jobs (
			id TEXT PRIMARY KEY,
			station TEXT NOT NULL,
			session TEXT NOT NULL,
			idem_key TEXT NOT NULL DEFAULT '',
			request_hash TEXT NOT NULL DEFAULT '',
			client_ref TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT 'sync',
			lines TEXT NOT NULL,
			cursor INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			result BLOB,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS ix_print_jobs_status ON print_jobs(status, created_at)`,
		`CREATE INDEX IF NOT EXISTS ix_print_jobs_idem ON print_jobs(idem_key) WHERE idem_key != ''`,
		`INSERT OR IGNORE INTO schema_meta(key, value) VALUES ('version', '3')`,
	}
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("aplicar esquema: %w", err)
		}
	}
	return nil
}

// RecordDocument guarda un documento. Devuelve duplicate=true si el número ya
// existía para ese tipo (doble emisión detectada), sin sobrescribir el original.
func (s *Store) RecordDocument(ctx context.Context, rec DocumentRecord) (DocumentRecord, bool, error) {
	if rec.IssuedAt.IsZero() {
		rec.IssuedAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rec, false, err
	}
	defer func() { _ = tx.Rollback() }()

	if rec.Number > 0 {
		var found DocumentRecord
		var issued string
		err := tx.QueryRowContext(ctx, `SELECT sequence, station_id, session_id, issued_at,
			request_sha256, response_sha256, response_text
			FROM documents WHERE doc_type = ? AND doc_number = ? LIMIT 1`,
			rec.Type, rec.Number).Scan(&found.Sequence, &found.StationID, &found.SessionID, &issued,
			&found.RequestSHA256, &found.ResponseSHA256, &found.ResponseText)
		switch {
		case err == nil:
			found.Type = rec.Type
			found.Number = rec.Number
			found.IssuedAt, _ = time.Parse(time.RFC3339Nano, issued)
			found.Duplicate = true
			found.ClientRef = rec.ClientRef
			return found, true, nil
		case errors.Is(err, sql.ErrNoRows):
			// número nuevo: continúa con el alta
		default:
			return rec, false, fmt.Errorf("buscar documento existente: %w", err)
		}
	}

	res, err := tx.ExecContext(ctx, `INSERT INTO documents
		(doc_type, doc_number, station_id, session_id, issued_at, status_byte, error_byte,
		 request_sha256, response_sha256, response_text, client_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Type, rec.Number, rec.StationID, rec.SessionID, rec.IssuedAt.Format(time.RFC3339Nano),
		int(rec.StatusByte), int(rec.ErrorByte), rec.RequestSHA256, rec.ResponseSHA256,
		rec.ResponseText, rec.ClientRef)
	if err != nil {
		return rec, false, fmt.Errorf("insertar documento: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return rec, false, err
	}
	rec.Sequence = seq
	if err := tx.Commit(); err != nil {
		return rec, false, err
	}
	return rec, false, nil
}

// LogCommand agrega una entrada a la bitácora de comandos.
func (s *Store) LogCommand(ctx context.Context, c CommandLog) error {
	if c.At.IsZero() {
		c.At = time.Now()
	}
	ok := 0
	if c.OK {
		ok = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO command_log
		(at, station_id, session_id, request_sha256, response_sha256, ok, latency_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.At.Format(time.RFC3339Nano), c.StationID, c.SessionID, c.RequestSHA256,
		c.ResponseSHA256, ok, c.LatencyMS, c.Error)
	return err
}

// Snapshot resume el historial persistido (correlativo y último documento).
func (s *Store) Snapshot(ctx context.Context) (History, error) {
	h := History{LastNumbers: map[string]int64{}}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(sequence), 0) FROM documents`).
		Scan(&h.Count, &h.LastSequence); err != nil {
		return h, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT doc_type, MAX(doc_number) FROM documents
		WHERE doc_number > 0 GROUP BY doc_type`)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var t string
		var n int64
		if err := rows.Scan(&t, &n); err != nil {
			_ = rows.Close()
			return h, err
		}
		h.LastNumbers[t] = n
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return h, err
	}
	_ = rows.Close()

	var issued string
	doc := DocumentRecord{}
	err = s.db.QueryRowContext(ctx, `SELECT sequence, doc_type, doc_number, station_id, session_id,
		issued_at, request_sha256, response_sha256, response_text
		FROM documents ORDER BY sequence DESC LIMIT 1`).
		Scan(&doc.Sequence, &doc.Type, &doc.Number, &doc.StationID, &doc.SessionID,
			&issued, &doc.RequestSHA256, &doc.ResponseSHA256, &doc.ResponseText)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return h, nil
	case err != nil:
		return h, err
	}
	doc.IssuedAt, _ = time.Parse(time.RFC3339Nano, issued)
	h.LastDocument = &doc
	return h, nil
}

// Documents devuelve los últimos documentos registrados (auditoría).
func (s *Store) Documents(ctx context.Context, limit int) ([]DocumentRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, doc_type, doc_number, station_id, session_id,
		issued_at, request_sha256, response_sha256, response_text
		FROM documents ORDER BY sequence DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DocumentRecord
	for rows.Next() {
		var d DocumentRecord
		var issued string
		if err := rows.Scan(&d.Sequence, &d.Type, &d.Number, &d.StationID, &d.SessionID,
			&issued, &d.RequestSHA256, &d.ResponseSHA256, &d.ResponseText); err != nil {
			return nil, err
		}
		d.IssuedAt, _ = time.Parse(time.RFC3339Nano, issued)
		out = append(out, d)
	}
	return out, rows.Err()
}

// SavePrinterInfo persiste la identificación de la impresora.
func (s *Store) SavePrinterInfo(ctx context.Context, info fiscal.Info) error {
	value := strings.Join([]string{
		info.Brand, info.Model, info.Serial, info.Firmware, info.RIF,
		strconv.FormatFloat(info.MemoryTotalMB, 'f', 2, 64),
		strconv.FormatFloat(info.MemoryFreeMB, 'f', 2, 64),
		strconv.FormatInt(info.LastInvoice, 10),
		strconv.FormatInt(info.LastCredit, 10),
		strconv.FormatInt(info.LastDebit, 10),
	}, "|")
	_, err := s.db.ExecContext(ctx, `INSERT INTO printer_state(key, value, updated_at) VALUES ('info', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		value, time.Now().Format(time.RFC3339Nano))
	return err
}

// LoadPrinterInfo recupera la identificación persistida de la impresora.
func (s *Store) LoadPrinterInfo(ctx context.Context) (fiscal.Info, bool) {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM printer_state WHERE key = 'info'`).Scan(&value); err != nil {
		return fiscal.Info{}, false
	}
	parts := strings.Split(value, "|")
	if len(parts) < 10 {
		return fiscal.Info{}, false
	}
	return fiscal.Info{
		Brand: parts[0], Model: parts[1], Serial: parts[2], Firmware: parts[3], RIF: parts[4],
		MemoryTotalMB: parseFloat(parts[5]), MemoryFreeMB: parseFloat(parts[6]),
		LastInvoice: parseI64(parts[7]), LastCredit: parseI64(parts[8]), LastDebit: parseI64(parts[9]),
	}, true
}

// PruneCommands elimina la bitácora anterior a la fecha indicada.
// IdempotencyGet devuelve la respuesta guardada para una clave de
// idempotencia REST. ok=false cuando la clave no existe.
func (s *Store) IdempotencyGet(key string) (requestHash string, status int, body []byte, ok bool, err error) {
	row := s.db.QueryRowContext(context.Background(),
		`SELECT request_hash, status, response FROM idempotency_keys WHERE key = ?`, key)
	err = row.Scan(&requestHash, &status, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil, false, nil
	}
	if err != nil {
		return "", 0, nil, false, fmt.Errorf("leer idempotencia: %w", err)
	}
	return requestHash, status, body, true, nil
}

// IdempotencyPut registra la respuesta de una petición ya ejecutada.
func (s *Store) IdempotencyPut(key, requestHash string, status int, body []byte) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO idempotency_keys(key, request_hash, status, response, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO NOTHING`,
		key, requestHash, status, body, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("guardar idempotencia: %w", err)
	}
	return nil
}

// IdempotencyReplace sobrescribe la respuesta de una clave: se usa cuando el
// trabajo pasó de "aceptado" (202) a su resultado final (201/4xx). Conserva el
// hash original para que la detección de conflicto siga vigente.
func (s *Store) IdempotencyReplace(key string, status int, body []byte) error {
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE idempotency_keys SET status = ?, response = ? WHERE key = ?`,
		status, body, key)
	if err != nil {
		return fmt.Errorf("finalizar idempotencia: %w", err)
	}
	return nil
}

// PruneIdempotency elimina claves anteriores a la fecha dada.
func (s *Store) PruneIdempotency(before time.Time) (int64, error) {
	res, err := s.db.ExecContext(context.Background(),
		`DELETE FROM idempotency_keys WHERE created_at < ?`, before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) PruneCommands(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM command_log WHERE at < ?`, before.Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- correlativo ----------

// SetCorrelative registra el último número emitido por tipo. La fuente es
// "printer" (S1 o respuesta de cierre) o "db" (auditoría propia). La impresora
// siempre gana: el valor se sobrescribe aunque sea menor (ej. tras un Z).
func (s *Store) SetCorrelative(ctx context.Context, docType string, number int64, source string) error {
	if source == "" {
		source = "db"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO correlative_state(doc_type, last_number, source, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(doc_type) DO UPDATE SET last_number = excluded.last_number,
			source = excluded.source, updated_at = excluded.updated_at`,
		docType, number, source, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("guardar correlativo %s: %w", docType, err)
	}
	return nil
}

// Correlatives devuelve el último número conocido por tipo de documento.
func (s *Store) Correlatives(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc_type, last_number FROM correlative_state`)
	if err != nil {
		return nil, fmt.Errorf("leer correlativos: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var t string
		var n int64
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, rows.Err()
}

// ---------- cola duradera ----------

// JobStatus son los estados posibles de un trabajo de impresión REST.
const (
	JobQueued   = "queued"   // aceptado, aún no llega a papel
	JobPrinting = "printing" // ejecutándose (inline o por el despachador)
	JobPaused   = "paused"   // detenido por impedimento transitorio (papel, offline)
	JobAborted  = "aborted"  // parcialmente impreso: exige re-sometido del integrador
	JobDone     = "done"     // completo
	JobCanceled = "canceled" // cancelado por el operador antes de imprimir
)

// PrintJob es una factura REST persistida: la fuente de verdad de la cola.
type PrintJob struct {
	ID          string    `json:"id"`
	Station     string    `json:"station"`
	Session     string    `json:"session"`
	IdemKey     string    `json:"-"`
	RequestHash string    `json:"-"`
	ClientRef   string    `json:"client_ref,omitempty"`
	Mode        string    `json:"-"` // sync | async
	Lines       []string  `json:"-"`
	Cursor      int       `json:"cursor"`
	Status      string    `json:"status"`
	Result      []byte    `json:"-"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ErrJobNotFound indica que el identificador no existe.
var ErrJobNotFound = errors.New("store: trabajo de impresión no encontrado")

func jobColumns() string {
	return `id, station, session, idem_key, request_hash, client_ref, mode, lines,
		cursor, status, result, attempts, last_error, created_at, updated_at`
}

func scanJob(row interface{ Scan(...any) error }) (PrintJob, error) {
	j := PrintJob{}
	var lines, createdAt, updatedAt string
	var result []byte
	err := row.Scan(&j.ID, &j.Station, &j.Session, &j.IdemKey, &j.RequestHash, &j.ClientRef,
		&j.Mode, &lines, &j.Cursor, &j.Status, &result, &j.Attempts, &j.LastError,
		&createdAt, &updatedAt)
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal([]byte(lines), &j.Lines); err != nil {
		return j, fmt.Errorf("líneas del trabajo %s: %w", j.ID, err)
	}
	j.Result = result
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	j.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return j, nil
}

// CreateJob persiste un trabajo nuevo en estado queued.
func (s *Store) CreateJob(ctx context.Context, j *PrintJob) error {
	lines, err := json.Marshal(j.Lines)
	if err != nil {
		return fmt.Errorf("serializar líneas: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if j.Mode == "" {
		j.Mode = "sync"
	}
	j.Status = JobQueued
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, now)
	j.UpdatedAt = j.CreatedAt
	_, err = s.db.ExecContext(ctx, `INSERT INTO print_jobs
		(id, station, session, idem_key, request_hash, client_ref, mode, lines, cursor, status,
		 attempts, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, '', ?, ?)`,
		j.ID, j.Station, j.Session, j.IdemKey, j.RequestHash, j.ClientRef, j.Mode,
		string(lines), j.Status, now, now)
	if err != nil {
		return fmt.Errorf("crear trabajo: %w", err)
	}
	return nil
}

// GetJob devuelve un trabajo por id.
func (s *Store) GetJob(ctx context.Context, id string) (PrintJob, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns()+` FROM print_jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PrintJob{}, ErrJobNotFound
	}
	return j, err
}

// ActiveJobByIdemKey devuelve el trabajo activo (no terminado) de una clave de
// idempotencia, para que un reintento no cree una segunda factura.
func (s *Store) ActiveJobByIdemKey(ctx context.Context, key string) (PrintJob, bool, error) {
	if key == "" {
		return PrintJob{}, false, nil
	}
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns()+` FROM print_jobs
		 WHERE idem_key = ? AND status IN (?, ?, ?) ORDER BY created_at DESC LIMIT 1`,
		key, JobQueued, JobPrinting, JobPaused))
	if errors.Is(err, sql.ErrNoRows) {
		return PrintJob{}, false, nil
	}
	if err != nil {
		return PrintJob{}, false, fmt.Errorf("buscar trabajo activo: %w", err)
	}
	return j, true, nil
}

// ClaimJob marca el trabajo como printing de forma atómica. Devuelve false si
// otro ejecutor se lo llevó antes.
func (s *Store) ClaimJob(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE print_jobs SET status = ?, updated_at = ?, attempts = attempts + 1
		 WHERE id = ? AND status IN (?, ?)`,
		JobPrinting, time.Now().UTC().Format(time.RFC3339Nano), id, JobQueued, JobPaused)
	if err != nil {
		return false, fmt.Errorf("tomar trabajo %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SaveJobCursor persiste el avance dentro de la factura (líneas ya impresas).
func (s *Store) SaveJobCursor(ctx context.Context, id string, cursor int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE print_jobs SET cursor = ?, updated_at = ? WHERE id = ?`,
		cursor, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("avance del trabajo %s: %w", id, err)
	}
	return nil
}

// SetJobStatus actualiza el estado final o intermedio del trabajo.
func (s *Store) SetJobStatus(ctx context.Context, id, status, lastError string, result []byte) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE print_jobs SET status = ?, last_error = ?, result = ?, updated_at = ? WHERE id = ?`,
		status, lastError, result, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("estado del trabajo %s: %w", id, err)
	}
	return nil
}

// ResumableJobs devuelve trabajos listos para (re)intentar: los pausados de
// cualquier modo y los encolados asíncronos. Los sincrónicos encolados aún no
// fueron abandonados por su cliente HTTP, así que no se tocan.
func (s *Store) ResumableJobs(ctx context.Context, limit int) ([]PrintJob, error) {
	if limit <= 0 {
		limit = 16
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns()+` FROM print_jobs
		 WHERE status = ? OR (status = ? AND mode = ?)
		 ORDER BY created_at LIMIT ?`,
		JobPaused, JobQueued, "async", limit)
	if err != nil {
		return nil, fmt.Errorf("listar trabajos reanudables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PrintJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// PausedJobsCount cuenta trabajos detenidos esperando recuperación.
func (s *Store) PausedJobsCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM print_jobs WHERE status IN (?, ?)`, JobPaused, JobQueued).Scan(&n)
	return n, err
}

// RecoverInterruptedJobs se ejecuta al arrancar: un trabajo que quedó
// "printing" tras un corte nunca sabe si llegó a papel. Si no imprimió nada
// vuelve a la cola; si imprimió algo se aborta (el integrador re-somete).
func (s *Store) RecoverInterruptedJobs(ctx context.Context) (resumed, aborted int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns()+` FROM print_jobs WHERE status = ?`, JobPrinting)
	if err != nil {
		return 0, 0, fmt.Errorf("buscar trabajos interrumpidos: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var interrupted []PrintJob
	for rows.Next() {
		j, serr := scanJob(rows)
		if serr != nil {
			return 0, 0, serr
		}
		interrupted = append(interrupted, j)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, j := range interrupted {
		if j.Cursor == 0 {
			if err := s.SetJobStatus(ctx, j.ID, JobQueued, "recuperado tras reinicio del servidor", nil); err != nil {
				return resumed, aborted, err
			}
			resumed++
		} else {
			if err := s.SetJobStatus(ctx, j.ID, JobAborted,
				"interrumpido con avance; exige re-sometido (nunca se reimprime a mitad)", nil); err != nil {
				return resumed, aborted, err
			}
			aborted++
		}
	}
	return resumed, aborted, nil
}

// ListJobs devuelve los últimos trabajos, opcionalmente filtrados por estado.
func (s *Store) ListJobs(ctx context.Context, status string, limit int) ([]PrintJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + jobColumns() + ` FROM print_jobs`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listar trabajos: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PrintJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// PruneJobs elimina trabajos terminados anteriores a la fecha dada.
func (s *Store) PruneJobs(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM print_jobs WHERE status IN (?, ?, ?) AND updated_at < ?`,
		JobDone, JobAborted, JobCanceled, before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Close cierra la base de datos.
func (s *Store) Close() error { return s.db.Close() }

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func parseI64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}
