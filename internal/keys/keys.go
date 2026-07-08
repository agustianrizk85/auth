// Package keys loads or generates the Ed25519 signing key pair used to sign
// access tokens. The private key is persisted as PKCS#8 PEM (mode 0600); the
// public key as PKIX PEM (mode 0644) so department backends can fetch it
// out-of-band. On first run a new pair is generated and written.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LoadOrCreate returns the Ed25519 private key from privPath, generating and
// persisting a new (private, public) pair when privPath does not yet exist. The
// bool reports whether a new pair was created.
func LoadOrCreate(privPath, pubPath string) (ed25519.PrivateKey, bool, error) {
	data, err := os.ReadFile(privPath)
	switch {
	case err == nil:
		priv, perr := parsePrivatePEM(data)
		if perr != nil {
			return nil, false, fmt.Errorf("parse private key %s: %w", privPath, perr)
		}
		return priv, false, nil
	case errors.Is(err, os.ErrNotExist):
		// fall through to generate
	default:
		return nil, false, fmt.Errorf("read private key %s: %w", privPath, err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, err
	}
	if err := writePEM(privPath, "PRIVATE KEY", mustMarshalPKCS8(priv), 0o600); err != nil {
		return nil, false, err
	}
	if err := writePEM(pubPath, "PUBLIC KEY", mustMarshalPKIX(pub), 0o644); err != nil {
		return nil, false, err
	}
	return priv, true, nil
}

func parsePrivatePEM(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 private key")
	}
	return priv, nil
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	out := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	return os.WriteFile(path, out, mode)
}

func mustMarshalPKCS8(priv ed25519.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		panic("keys: marshal PKCS8: " + err.Error())
	}
	return der
}

func mustMarshalPKIX(pub ed25519.PublicKey) []byte {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		panic("keys: marshal PKIX: " + err.Error())
	}
	return der
}
