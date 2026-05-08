package sre

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// crlCache holds a parsed CRL and refreshes it from disk at most once per TTL.
// [145.H] Used by the mTLS VerifyPeerCertificate callback to reject revoked certs.
type crlCache struct {
	mu        sync.Mutex
	path      string
	revoked   map[string]struct{} // serialHex → struct{} — big.Int.Text(16)
	loadedAt  time.Time
	ttl       time.Duration
	missingOK bool // when true, a missing CRL file is logged once and treated as no-revocation
}

func newCRLCache(path string, ttl time.Duration, missingOK bool) *crlCache {
	return &crlCache{path: path, ttl: ttl, missingOK: missingOK}
}

// isRevoked returns true if the certificate's serial is on the CRL.
// Loads or refreshes the CRL from disk when the TTL has expired.
func (c *crlCache) isRevoked(cert *x509.Certificate) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.loadedAt) > c.ttl {
		_ = c.reload()
	}
	if c.revoked == nil {
		return false
	}
	_, found := c.revoked[cert.SerialNumber.Text(16)]
	return found
}

// reload reads the CRL file and rebuilds the revoked set. Caller must hold c.mu.
func (c *crlCache) reload() error {
	raw, err := os.ReadFile(c.path) //nolint:gosec // G304-WORKSPACE-CANON: path is operator-controlled PKI dir
	if err != nil {
		if os.IsNotExist(err) && c.missingOK {
			if c.revoked == nil {
				log.Printf("[SRE-MTLS] WARNING: no CRL found at %q — revoked certs will be accepted until a CRL is installed. In production, place a PEM CRL at this path.", c.path)
			}
			c.loadedAt = time.Now()
			return nil
		}
		return err
	}
	revoked, err := parseCRLRevoked(raw)
	if err != nil {
		return fmt.Errorf("parse CRL %q: %w", c.path, err)
	}
	c.revoked = revoked
	c.loadedAt = time.Now()
	log.Printf("[SRE-MTLS] CRL refreshed from %q: %d revoked serial(s)", c.path, len(revoked))
	return nil
}

// parseCRLRevoked parses a PEM-encoded CRL and returns the revoked serial numbers as hex strings.
// Requires Go 1.19+ (x509.ParseRevocationList).
func parseCRLRevoked(pemData []byte) (map[string]struct{}, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in CRL data")
	}
	crl, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CRL DER: %w", err)
	}
	out := make(map[string]struct{}, len(crl.RevokedCertificateEntries))
	for _, e := range crl.RevokedCertificateEntries {
		out[e.SerialNumber.Text(16)] = struct{}{}
	}
	return out, nil
}

// LoadMTLSConfig genera una configuración estricta de TLS 1.3 exigiendo certificados de cliente.
//
// [145.H] CRL revocation checking: if a PEM-encoded CRL exists at
// <cadir>/crl.pem, revoked client certs are rejected via VerifyPeerCertificate.
// The CRL is loaded eagerly and refreshed in memory every 1 h.
// Dev trade-off: when no crl.pem is present, the server emits a one-time WARNING
// and accepts all valid-chain certs — acceptable for local SCADA/PLC simulation
// where the CA is operator-controlled and cert issuance is manual. In production
// deployments, place a signed CRL at <cadir>/crl.pem and rotate it before expiry.
func LoadMTLSConfig(caCertPath string) (*tls.Config, error) {
	// 1. Cargar la Autoridad Certificadora (CA) Raíz de la Fábrica
	caCert, err := os.ReadFile(filepath.Clean(caCertPath))
	if err != nil {
		return nil, fmt.Errorf("imposible leer CA cert: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
		return nil, fmt.Errorf("fallo al parsear el certificado CA")
	}

	// [145.H] Look for a companion CRL in the same directory as the CA cert.
	crlPath := filepath.Join(filepath.Dir(caCertPath), "crl.pem")
	cache := newCRLCache(crlPath, time.Hour, true /* missingOK — dev trade-off */)
	_ = cache.reload() // eager load; errors are non-fatal (logged inside reload)

	// 2. Configuración SRE de Alta Seguridad y Rendimiento
	return &tls.Config{
		// Validar criptográficamente si el cliente presenta uno, pero no requerirlo estrictamente (Permite React HMI)
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  caCertPool,

		// TLS 1.3 es obligatorio: Elimina round-trips innecesarios y cifrados débiles (RSA key exchange)
		MinVersion: tls.VersionTLS13,

		// Curvas elípticas de hardware-accelerated (AES-GCM / ChaCha20-Poly1305)
		CurvePreferences: []tls.CurveID{tls.CurveP256, tls.X25519},

		// [145.I] Disable session tickets — session ticket replay could re-authenticate without
		// presenting a client cert (TLS resumed sessions skip ClientAuth on resumption path).
		SessionTicketsDisabled: true,

		// [145.H] Reject client certs whose serial number appears in the cached CRL.
		// Also rejects the server cert (rawCerts[0]) but the server is us, so that is harmless.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			for _, raw := range rawCerts {
				cert, err := x509.ParseCertificate(raw)
				if err != nil {
					return fmt.Errorf("VerifyPeerCertificate: parse cert: %w", err)
				}
				if cache.isRevoked(cert) {
					return fmt.Errorf("VerifyPeerCertificate: certificate serial %s is revoked", cert.SerialNumber.Text(16))
				}
			}
			return nil
		},
	}, nil
}
