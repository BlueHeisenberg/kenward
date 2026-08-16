package dashboard

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"runtime"
	"strings"
	"time"
)

// CertLifetime is how long a generated certificate is valid for.
//
// 825 days, which is the longest a self-signed leaf is accepted for by the browsers that
// enforce a maximum at all. A shorter one would mean the household clicking through a
// new warning — and, worse, checking a new fingerprint — on a schedule nobody set.
const CertLifetime = 825 * 24 * time.Hour

// generateSelfSigned writes a certificate and key for host and returns the
// certificate's SHA-256 fingerprint in the form a browser shows it.
//
// The fingerprint is the whole point of returning anything. A self-signed certificate
// produces a warning page, and a warning page clicked through without checking anything
// is worth nothing at all: what makes this arrangement real is that the dashboard showed
// the fingerprint over a connection the operator already trusted — loopback, or their
// tailnet — and the browser shows the same one afterwards. Matching them once is the
// entire ceremony, and it is why this is shown rather than logged.
//
// P-256 rather than RSA: it is faster to generate on the small machines this runs on,
// every browser has taken it for a decade, and there is nothing here that needs to
// interoperate with anything older.
func generateSelfSigned(certFile, keyFile, host string, now time.Time) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("dashboard: generating a key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", fmt.Errorf("dashboard: generating a serial: %w", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"kenward"}},
		// Backdated an hour so a household whose clock is a few minutes fast does
		// not meet a certificate that is not valid yet — which browsers report as
		// a different and much more alarming error than an untrusted issuer.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CertLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	// Both forms, and loopback alongside whatever was asked for: the operator will
	// keep reaching this from the machine itself, and a certificate that is only
	// valid for the LAN address turns that into a second warning.
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else if host != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
	tmpl.DNSNames = append(tmpl.DNSNames, "localhost")
	if name, err := os.Hostname(); err == nil && name != "" && !strings.EqualFold(name, host) {
		tmpl.DNSNames = append(tmpl.DNSNames, name)
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return "", fmt.Errorf("dashboard: signing the certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("dashboard: encoding the key: %w", err)
	}

	// The key first and at 0600. If the write of the certificate fails afterwards
	// the pair is unusable and the operator tries again, which is a better failure
	// than a certificate on disk with a world-readable key beside it.
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), fileMode); err != nil {
		return "", fmt.Errorf("dashboard: writing %s: %w", keyFile, err)
	}
	// Chmod as well as passing the mode to WriteFile: the mode argument only applies
	// when the file is created, so regenerating over a key that somehow ended up
	// world-readable would leave it that way. Windows has no mode bits worth the name
	// and failing there would make LAN exposure unusable on it.
	if err := os.Chmod(keyFile, fileMode); err != nil && runtime.GOOS != "windows" {
		return "", fmt.Errorf("dashboard: setting permissions on %s: %w", keyFile, err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", fmt.Errorf("dashboard: writing %s: %w", certFile, err)
	}
	return fingerprint(der), nil
}

// fingerprint renders a certificate's SHA-256 the way a browser does: uppercase hex,
// colon-separated. The formatting is not decoration — it is what makes a by-eye
// comparison against the browser's certificate viewer possible at all.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	hexed := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(hexed); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexed[i : i+2])
	}
	return b.String()
}
