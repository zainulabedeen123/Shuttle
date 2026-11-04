package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// loadOrCreateIdentity returns this device's TLS certificate and its fingerprint.
// The certificate is self-signed; trust comes from pinning fingerprints at pairing time.
func loadOrCreateIdentity(home string) (tls.Certificate, string, error) {
	certPath := filepath.Join(home, "cert.pem")
	keyPath := filepath.Join(home, "key.pem")
	if _, err := os.Stat(certPath); err == nil {
		if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			return cert, fingerprint(cert.Certificate[0]), nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "shuttle-device"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(30, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, fingerprint(der), nil
}

func fingerprint(der []byte) string {
	s := sha256.Sum256(der)
	return hex.EncodeToString(s[:])
}

// pairCode derives the 6-digit code both devices display during pairing.
// It depends on both certificates, so a man-in-the-middle produces different codes.
func pairCode(fpA, fpB string) string {
	if fpA > fpB {
		fpA, fpB = fpB, fpA
	}
	h := sha256.Sum256([]byte("shuttle-pair:" + fpA + ":" + fpB))
	n := binary.BigEndian.Uint32(h[:4]) % 1000000
	return fmt.Sprintf("%03d %03d", n/1000, n%1000)
}
