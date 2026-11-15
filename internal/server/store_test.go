package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistsDocumentsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spooler.db")
	ctx := context.Background()

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	rec := DocumentRecord{
		Type: "invoice", Number: 1, StationID: "CAJA-01", SessionID: "s1",
		IssuedAt: time.Now(), RequestSHA256: "req", ResponseSHA256: "resp",
		ResponseText: "0|Factura 1|Total: 116.00",
	}
	saved, dup, err := store.RecordDocument(ctx, rec)
	if err != nil {
		t.Fatalf("RecordDocument: %v", err)
	}
	if dup {
		t.Fatal("no debía ser duplicado")
	}
	if saved.Sequence != 1 {
		t.Fatalf("secuencia = %d", saved.Sequence)
	}

	snap, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Count != 1 || snap.LastNumbers["invoice"] != 1 {
		t.Fatalf("historial = %+v", snap)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reabrir: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	snap, err = reopened.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot tras reinicio: %v", err)
	}
	if snap.Count != 1 || snap.LastSequence != 1 {
		t.Fatalf("el historial no se conservó: %+v", snap)
	}
	if snap.LastDocument == nil || snap.LastDocument.Number != 1 {
		t.Fatalf("último documento = %+v", snap.LastDocument)
	}
	if snap.LastDocument.IssuedAt.IsZero() {
		t.Fatal("la fecha no se conservó")
	}
}

func TestStoreDetectsDuplicateDocumentNumber(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "spooler.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	first := DocumentRecord{Type: "invoice", Number: 7, StationID: "CAJA-01", SessionID: "s1",
		RequestSHA256: "r1", ResponseSHA256: "x1", ResponseText: "0|Factura 7"}
	if _, dup, err := store.RecordDocument(ctx, first); err != nil || dup {
		t.Fatalf("primer alta: dup=%v err=%v", dup, err)
	}

	second := first
	second.StationID = "CAJA-02"
	second.ResponseText = "0|Factura 7 (reimpresa)"
	existing, dup, err := store.RecordDocument(ctx, second)
	if err != nil {
		t.Fatalf("segundo alta: %v", err)
	}
	if !dup {
		t.Fatal("debía detectar duplicado")
	}
	if existing.StationID != "CAJA-01" {
		t.Fatalf("se sobrescribió el documento original: %+v", existing)
	}

	snap, _ := store.Snapshot(ctx)
	if snap.Count != 1 {
		t.Fatalf("documentos = %d, se esperaba 1", snap.Count)
	}
}

func TestStoreCommandLogAndPrune(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "spooler.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	old := CommandLog{At: time.Now().AddDate(0, 0, -30), StationID: "CAJA-01", OK: true}
	fresh := CommandLog{At: time.Now(), StationID: "CAJA-01", OK: false, Error: "x"}
	if err := store.LogCommand(ctx, old); err != nil {
		t.Fatalf("LogCommand: %v", err)
	}
	if err := store.LogCommand(ctx, fresh); err != nil {
		t.Fatalf("LogCommand: %v", err)
	}
	n, err := store.PruneCommands(ctx, time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("PruneCommands: %v", err)
	}
	if n != 1 {
		t.Fatalf("purgadas = %d, se esperaba 1", n)
	}
}

// ---------- correlativo persistente ----------

func TestStoreCorrelativePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spooler.db")
	ctx := context.Background()

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.SetCorrelative(ctx, "invoice", 41, "printer"); err != nil {
		t.Fatalf("SetCorrelative: %v", err)
	}
	// La impresora siempre gana, incluso con un número menor (ej. tras un Z).
	if err := store.SetCorrelative(ctx, "invoice", 39, "printer"); err != nil {
		t.Fatalf("SetCorrelative menor: %v", err)
	}
	if err := store.SetCorrelative(ctx, "credit", 3, ""); err != nil {
		t.Fatalf("SetCorrelative credit: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reabrir: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	rels, err := reopened.Correlatives(ctx)
	if err != nil {
		t.Fatalf("Correlatives: %v", err)
	}
	if rels["invoice"] != 39 || rels["credit"] != 3 {
		t.Fatalf("correlativos = %v", rels)
	}
}

// ---------- cola duradera de trabajos ----------

func TestStoreJobLifecycleAndRecovery(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "spooler.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	job := &PrintJob{
		ID: "job_1", Station: "WEB-01", Session: "api:WEB-01",
		IdemKey: "k1", RequestHash: "h1", ClientRef: "ref-1",
		Mode: "sync", Lines: []string{"!item", "101"},
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.Status != JobQueued {
		t.Fatalf("estado inicial = %s", job.Status)
	}

	got, err := store.GetJob(ctx, "job_1")
	if err != nil || len(got.Lines) != 2 || got.Lines[1] != "101" {
		t.Fatalf("GetJob = %+v (%v)", got, err)
	}

	// Idempotencia: el trabajo activo es visible por clave.
	active, ok, err := store.ActiveJobByIdemKey(ctx, "k1")
	if err != nil || !ok || active.ID != "job_1" {
		t.Fatalf("ActiveJobByIdemKey = %+v %v (%v)", active, ok, err)
	}
	if _, ok, _ := store.ActiveJobByIdemKey(ctx, "k-otra"); ok {
		t.Fatal("clave sin trabajo no debía tener activo")
	}

	// Claim atómico: el segundo intento falla.
	if claimed, err := store.ClaimJob(ctx, "job_1"); err != nil || !claimed {
		t.Fatalf("ClaimJob = %v %v", claimed, err)
	}
	if claimed, _ := store.ClaimJob(ctx, "job_1"); claimed {
		t.Fatal("el segundo claim debía fallar (ya printing)")
	}

	// Avance y pausa.
	if err := store.SaveJobCursor(ctx, "job_1", 1); err != nil {
		t.Fatalf("SaveJobCursor: %v", err)
	}
	if err := store.SetJobStatus(ctx, "job_1", JobPaused, "PAPER_OUT", nil); err != nil {
		t.Fatalf("SetJobStatus: %v", err)
	}

	// Los pausados son reanudables.
	resumable, err := store.ResumableJobs(ctx, 10)
	if err != nil || len(resumable) != 1 || resumable[0].Cursor != 1 {
		t.Fatalf("ResumableJobs = %+v (%v)", resumable, err)
	}

	// Un trabajo encolado sincrónico NO es reanudable (su cliente está ahí).
	syncQueued := &PrintJob{ID: "job_2", Station: "WEB-01", Session: "api:WEB-01", Mode: "sync", Lines: []string{"!i", "101"}}
	if err := store.CreateJob(ctx, syncQueued); err != nil {
		t.Fatalf("CreateJob 2: %v", err)
	}
	if res, _ := store.ResumableJobs(ctx, 10); len(res) != 1 {
		t.Fatalf("el sync encolado no debía ser reanudable: %+v", res)
	}
	// El asíncrono encolado sí lo es.
	asyncQueued := &PrintJob{ID: "job_3", Station: "WEB-02", Session: "api:WEB-02", Mode: "async", Lines: []string{"!i", "101"}}
	if err := store.CreateJob(ctx, asyncQueued); err != nil {
		t.Fatalf("CreateJob 3: %v", err)
	}
	if res, _ := store.ResumableJobs(ctx, 10); len(res) != 2 {
		t.Fatalf("resumables = %d, se esperaban 2", len(res))
	}

	// Recuperación al arrancar: sin avance vuelve a la cola; con avance aborta.
	printing0 := &PrintJob{ID: "job_4", Station: "W", Session: "api:W", Mode: "async", Lines: []string{"!i", "101"}}
	if err := store.CreateJob(ctx, printing0); err != nil {
		t.Fatalf("CreateJob 4: %v", err)
	}
	if _, err := store.ClaimJob(ctx, "job_4"); err != nil {
		t.Fatal(err)
	}
	printing1 := &PrintJob{ID: "job_5", Station: "W", Session: "api:W", Mode: "async", Lines: []string{"!i", "!i", "101"}}
	if err := store.CreateJob(ctx, printing1); err != nil {
		t.Fatalf("CreateJob 5: %v", err)
	}
	if _, err := store.ClaimJob(ctx, "job_5"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJobCursor(ctx, "job_5", 2); err != nil {
		t.Fatal(err)
	}

	resumed, aborted, err := store.RecoverInterruptedJobs(ctx)
	if err != nil {
		t.Fatalf("RecoverInterruptedJobs: %v", err)
	}
	if resumed != 1 || aborted != 1 {
		t.Fatalf("recuperación: resumed=%d aborted=%d", resumed, aborted)
	}
	j4, _ := store.GetJob(ctx, "job_4")
	j5, _ := store.GetJob(ctx, "job_5")
	if j4.Status != JobQueued {
		t.Fatalf("job_4 debía volver a la cola: %s", j4.Status)
	}
	if j5.Status != JobAborted {
		t.Fatalf("job_5 debía abortarse con avance: %s", j5.Status)
	}

	// Idempotencia: provisional 202 → definitiva 201; conflicto de cuerpo intacto.
	if err := store.IdempotencyPut("k1", "h1", 202, []byte(`{"job_id":"job_1"}`)); err != nil {
		t.Fatalf("IdempotencyPut: %v", err)
	}
	if err := store.IdempotencyReplace("k1", 201, []byte(`{"completed":true}`)); err != nil {
		t.Fatalf("IdempotencyReplace: %v", err)
	}
	_, status, body, ok, err := store.IdempotencyGet("k1")
	if err != nil || !ok || status != 201 || string(body) != `{"completed":true}` {
		t.Fatalf("IdempotencyGet tras reemplazo: status=%d ok=%v body=%s (%v)", status, ok, body, err)
	}
	if h, _, _, ok, _ := store.IdempotencyGet("k1"); !ok || h != "h1" {
		t.Fatalf("el hash original debía conservarse: %q", h)
	}

	// Purga de terminados (done y aborted).
	if err := store.SetJobStatus(ctx, "job_1", JobDone, "", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PruneJobs(ctx, time.Now().Add(time.Minute)); err != nil || n != 2 {
		t.Fatalf("PruneJobs = %d (%v)", n, err)
	}
	if _, err := store.GetJob(ctx, "job_1"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("job_1 debía purgarse: %v", err)
	}
}
