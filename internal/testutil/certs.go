// Paquete testutil: generación de certificados para las pruebas de TLS/mTLS.
// No forma parte de los binarios de producción.
package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// CertBundle son las rutas de los certificados de prueba.
type CertBundle struct {
	CAFile     string
	ServerCert string
	ServerKey  string
	ClientCert string
	ClientKey  string
}

// WriteCertificates genera una CA, un certificado de servidor para 127.0.0.1 y
// uno de cliente, y los escribe en dir. Sirve para probar TLS y mTLS reales.
func WriteCertificates(dir string) (CertBundle, error) {
	var b CertBundle
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return b, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Spooler Test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return b, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return b, err
	}

	serverCert, serverKey, err := issueCert(caCert, caKey, "spooler-server", []x509.ExtKeyUsage{
		x509.ExtKeyUsageServerAuth,
	}, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		return b, err
	}
	clientCert, clientKey, err := issueCert(caCert, caKey, "agente-caja", []x509.ExtKeyUsage{
		x509.ExtKeyUsageClientAuth,
	}, nil, nil)
	if err != nil {
		return b, err
	}

	b.CAFile = filepath.Join(dir, "ca.crt")
	b.ServerCert = filepath.Join(dir, "server.crt")
	b.ServerKey = filepath.Join(dir, "server.key")
	b.ClientCert = filepath.Join(dir, "client.crt")
	b.ClientKey = filepath.Join(dir, "client.key")
	if err := writePEM(b.CAFile, "CERTIFICATE", caDER); err != nil {
		return b, err
	}
	if err := writePEM(b.ServerCert, "CERTIFICATE", serverCert); err != nil {
		return b, err
	}
	if err := writePEM(b.ServerKey, "EC PRIVATE KEY", serverKey); err != nil {
		return b, err
	}
	if err := writePEM(b.ClientCert, "CERTIFICATE", clientCert); err != nil {
		return b, err
	}
	return b, writePEM(b.ClientKey, "EC PRIVATE KEY", clientKey)
}

func issueCert(ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string,
	usage []x509.ExtKeyUsage, dns []string, ips []net.IP) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usage,
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return der, keyDER, nil
}

func writePEM(path, blockType string, der []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("crear %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}
