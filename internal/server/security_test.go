package server

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"testing"
	"time"

	"spoolerfiscal/internal/config"
	"spoolerfiscal/internal/testutil"
)

// Un token de estación sólo puede emitir en su propia estación; el token
// compartido no representa a una estación que tiene token propio.
func TestStationTokensBindStation(t *testing.T) {
	srv, _, _, _ := testAPI(t, HTTPConfig{
		Token: "compartido",
		StationTokens: map[string]string{
			"CAJA-01": "token-caja-01",
		},
	})

	post := func(token, station string) int {
		t.Helper()
		payload := mustJSON(t, map[string]any{"station": station, "command": "S2"})
		resp, _ := httpDo(t, srv, http.MethodPost, "/v1/commands", token, payload, nil)
		return resp.StatusCode
	}

	if got := post("token-caja-01", "CAJA-01"); got != http.StatusOK {
		t.Fatalf("token propio en su estación = %d", got)
	}
	if got := post("token-caja-01", "CAJA-02"); got != http.StatusForbidden {
		t.Fatalf("token propio en otra estación = %d (se esperaba 403)", got)
	}
	if got := post("compartido", "CAJA-01"); got != http.StatusForbidden {
		t.Fatalf("token compartido en estación con token propio = %d (se esperaba 403)", got)
	}
	if got := post("compartido", "CAJA-02"); got != http.StatusOK {
		t.Fatalf("token compartido en estación sin token propio = %d", got)
	}
	if got := post("token-invalido", "CAJA-01"); got != http.StatusUnauthorized {
		t.Fatalf("token inválido = %d (se esperaba 401)", got)
	}

	// El encabezado alternativo también queda atado a la estación.
	payload := mustJSON(t, map[string]any{"station": "CAJA-02", "command": "S2"})
	resp, _ := httpDo(t, srv, http.MethodPost, "/v1/commands", "", payload,
		map[string]string{"X-Spooler-Token": "token-caja-01"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("X-Spooler-Token en otra estación = %d", resp.StatusCode)
	}

	// Lectura: cualquier token válido sirve.
	resp, _ = httpDo(t, srv, http.MethodGet, "/v1/status", "token-caja-01", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lectura con token de estación = %d", resp.StatusCode)
	}
}

// ca_file activa mTLS: sin certificado de cliente el handshake falla, con él
// funciona. Se prueba contra un listener TLS real.
func TestHTTPTLSConfigMTLS(t *testing.T) {
	bundle, err := testutil.WriteCertificates(t.TempDir())
	if err != nil {
		t.Fatalf("WriteCertificates: %v", err)
	}
	tlsCfg, err := httpTLSConfig(config.TLS{
		Enabled:  true,
		CAFile:   bundle.CAFile,
		CertFile: bundle.ServerCert,
		KeyFile:  bundle.ServerKey,
	})
	if err != nil {
		t.Fatalf("httpTLSConfig: %v", err)
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v", tlsCfg.ClientAuth)
	}
	if tlsCfg.ClientCAs == nil {
		t.Fatal("ClientCAs no configurado")
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	caPEM, err := os.ReadFile(bundle.CAFile)
	if err != nil {
		t.Fatalf("leer CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA inválida")
	}
	clientCert, err := tls.LoadX509KeyPair(bundle.ClientCert, bundle.ClientKey)
	if err != nil {
		t.Fatalf("certificado de cliente: %v", err)
	}
	url := "https://" + ln.Addr().String()

	withCert := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: pool, Certificates: []tls.Certificate{clientCert}, ServerName: "127.0.0.1",
		},
	}}
	resp, err := withCert.Get(url)
	if err != nil {
		t.Fatalf("con certificado de cliente: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("con certificado: estado = %d", resp.StatusCode)
	}

	withoutCert := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
	}}
	if _, err := withoutCert.Get(url); err == nil {
		t.Fatal("sin certificado de cliente el handshake debía fallar")
	}
}

// Sin ca_file, TLS es de un solo sentido (el token autentica al cliente).
func TestHTTPTLSConfigOneWayByDefault(t *testing.T) {
	bundle, err := testutil.WriteCertificates(t.TempDir())
	if err != nil {
		t.Fatalf("WriteCertificates: %v", err)
	}
	tlsCfg, err := httpTLSConfig(config.TLS{
		Enabled:  true,
		CertFile: bundle.ServerCert,
		KeyFile:  bundle.ServerKey,
	})
	if err != nil {
		t.Fatalf("httpTLSConfig: %v", err)
	}
	if tlsCfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v (se esperaba NoClientCert)", tlsCfg.ClientAuth)
	}
}

func TestHTTPTLSConfigRejectsBadCA(t *testing.T) {
	bundle, err := testutil.WriteCertificates(t.TempDir())
	if err != nil {
		t.Fatalf("WriteCertificates: %v", err)
	}
	bad := bundle.CAFile + ".bad"
	if err := os.WriteFile(bad, []byte("no es una CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := httpTLSConfig(config.TLS{
		Enabled:  true,
		CAFile:   bad,
		CertFile: bundle.ServerCert,
		KeyFile:  bundle.ServerKey,
	}); err == nil {
		t.Fatal("una CA inválida debía rechazarse")
	}
}
