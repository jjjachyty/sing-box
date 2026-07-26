package agent

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// realityKeyPair holds the X25519 keypair and short ID for VLESS+Reality.
type realityKeyPair struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
	ShortID    string `json:"short_id"`
}

// isRealityEnabled reports whether the protocols config asks for VLESS+Reality.
func isRealityEnabled(protocols map[string]interface{}) bool {
	if protocols == nil {
		return false
	}
	if s, _ := protocols["security"].(string); s == "reality" {
		return true
	}
	if d, _ := protocols["reality_dest"].(string); d != "" {
		return true
	}
	return protocolBool(protocols, "reality")
}

// ensureRealityKeys loads the reality keypair from disk, generating and
// persisting a new one on first run. If reality_private_key is set in
// node.yaml it takes precedence and the public key is derived from it.
func ensureRealityKeys(path string, protocols map[string]interface{}) (*realityKeyPair, error) {
	// Private key explicitly configured in node.yaml wins
	if priv, _ := protocols["reality_private_key"].(string); priv != "" {
		pub, err := realityPublicKey(priv)
		if err != nil {
			return nil, fmt.Errorf("invalid reality_private_key: %w", err)
		}
		sid, _ := protocols["reality_short_id"].(string)
		if sid == "" {
			sid = randomShortID()
		}
		return &realityKeyPair{PrivateKey: priv, PublicKey: pub, ShortID: sid}, nil
	}

	if data, err := os.ReadFile(path); err == nil {
		var k realityKeyPair
		if err := json.Unmarshal(data, &k); err == nil && k.PrivateKey != "" && k.PublicKey != "" {
			return &k, nil
		}
	}

	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	k := &realityKeyPair{
		PrivateKey: base64.RawURLEncoding.EncodeToString(priv.Bytes()),
		PublicKey:  base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		ShortID:    randomShortID(),
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return nil, err
	}
	return k, nil
}

// realityPublicKey derives the base64url public key from a base64url X25519 private key.
func realityPublicKey(privB64 string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(privB64)
	if err != nil {
		return "", err
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}

func randomShortID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// ensureSelfSignedCert generates a self-signed RSA certificate if the
// given cert/key files do not exist yet.
func ensureSelfSignedCert(certPath, keyPath string) error {
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return nil // both exist
		}
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return err
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "vpn-node"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}

	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return err
	}

	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer keyOut.Close()
	return pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
