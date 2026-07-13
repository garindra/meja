package client

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestPinnedTLSRequiresExactSPKI(t *testing.T) {
	spki := []byte("test spki")
	hash := sha256.Sum256(spki)
	config, err := loadTLSConfig(hexHash(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	if !config.InsecureSkipVerify || config.VerifyConnection == nil {
		t.Fatal("pinning configuration is not mandatory")
	}
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{{RawSubjectPublicKeyInfo: spki}}}); err != nil {
		t.Fatal(err)
	}
	wrong := []byte("wrong")
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{{RawSubjectPublicKeyInfo: wrong}}}); err == nil {
		t.Fatal("accepted wrong SPKI")
	}
}

func hexHash(data []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(data)*2)
	for i, b := range data {
		out[2*i], out[2*i+1] = digits[b>>4], digits[b&15]
	}
	return string(out)
}
