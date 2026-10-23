package bevdirect

// Upstream TLS trust and upstream health for the BEV tile host.
//
// Since 2026-10-23 kataster.bev.gv.at serves a *.bev.gv.at leaf issued by
// "GEANT TLS RSA 1" (HARICA) WITHOUT sending the intermediate, so a plain
// Go/OpenSSL client fails with "certificate signed by unknown authority".
//
// Verification here is standard chain building against the system roots
// (SSL_CERT_FILE / SSL_CERT_DIR honoured by Go) — nothing is skipped, the
// hostname is enforced — with three sources of intermediates, in order:
//
//  1. whatever the server sends (so when BEV fixes its chain, or changes CA,
//     nothing here matters any more);
//  2. the PEMs shipped in certs/ (GEANT TLS RSA 1, AIA
//     http://crt.harica.gr/HARICA-GEANT-TLS-R1.cer) and any extra file given
//     with -ca-bundle / BEVDIRECT_CA_BUNDLE (roots or intermediates);
//  3. on "unknown authority" only: the issuer certificate named in the leaf's
//     AIA "CA Issuers" URL, fetched once over plain HTTP (it is signed, so
//     transport integrity is irrelevant) and cached for the process lifetime.
//     This covers the next time any CA's intermediate goes missing.
//
// An expired or superseded embedded intermediate is harmless: it simply never
// takes part in a valid chain.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed certs/*.pem
var embeddedCerts embed.FS

// EmbeddedIntermediates returns the CA certificates shipped in certs/.
func EmbeddedIntermediates() []*x509.Certificate {
	var out []*x509.Certificate
	ents, _ := embeddedCerts.ReadDir("certs")
	for _, e := range ents {
		if b, err := embeddedCerts.ReadFile("certs/" + e.Name()); err == nil {
			out = append(out, ParsePEMCerts(b)...)
		}
	}
	return out
}

// ParsePEMCerts parses every CERTIFICATE block in b, skipping bad ones.
func ParsePEMCerts(b []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return out
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
			out = append(out, c)
		}
	}
}

// Verifier holds the trust material for BEV connections.
type Verifier struct {
	roots, inter *x509.CertPool
	aia          sync.Map // url → *x509.Certificate (nil = tried and failed; retried after aiaRetry)
	aiaAt        sync.Map // url → time.Time of last attempt
	AIAFetch     bool     // fetch missing issuers from the leaf's AIA URL (default on)
	fetched      atomic.Int64
}

const aiaRetry = 10 * time.Minute

// NewVerifier loads system roots + embedded intermediates (+ caBundle, or
// $BEVDIRECT_CA_BUNDLE). A missing/invalid bundle is an error.
func NewVerifier(caBundle string) (*Verifier, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	v := &Verifier{roots: roots, inter: x509.NewCertPool(), AIAFetch: os.Getenv("BEVDIRECT_NO_AIA") == ""}
	for _, c := range EmbeddedIntermediates() {
		v.inter.AddCert(c)
	}
	if caBundle == "" {
		caBundle = os.Getenv("BEVDIRECT_CA_BUNDLE")
	}
	if caBundle != "" {
		b, err := os.ReadFile(caBundle)
		if err != nil {
			return nil, fmt.Errorf("ca bundle: %w", err)
		}
		cs := ParsePEMCerts(b)
		if len(cs) == 0 {
			return nil, fmt.Errorf("ca bundle %s: no certificates", caBundle)
		}
		for _, c := range cs {
			v.roots.AddCert(c) // a self-signed entry acts as a root, an intermediate as an intermediate
			v.inter.AddCert(c)
		}
	}
	return v, nil
}

// Verify checks a presented chain for serverName (crypto/tls semantics).
func (v *Verifier) Verify(serverName string, chain []*x509.Certificate) error {
	if len(chain) == 0 {
		return errors.New("tls: no peer certificate")
	}
	leaf := chain[0]
	im := v.inter.Clone()
	for _, c := range chain[1:] {
		im.AddCert(c)
	}
	opts := x509.VerifyOptions{DNSName: serverName, Roots: v.roots, Intermediates: im,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	_, err := leaf.Verify(opts)
	if err == nil {
		return nil
	}
	var uae x509.UnknownAuthorityError
	if v.AIAFetch && errors.As(err, &uae) {
		for _, c := range v.issuersViaAIA(leaf) {
			im.AddCert(c)
		}
		if _, err2 := leaf.Verify(opts); err2 == nil {
			return nil
		}
	}
	return &tls.CertificateVerificationError{UnverifiedCertificates: chain, Err: err}
}

// issuersViaAIA fetches (once, cached) the CA Issuers URLs of c.
func (v *Verifier) issuersViaAIA(c *x509.Certificate) []*x509.Certificate {
	var out []*x509.Certificate
	for _, u := range c.IssuingCertificateURL {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			continue
		}
		if cached, ok := v.aia.Load(u); ok {
			if cc, _ := cached.(*x509.Certificate); cc != nil {
				out = append(out, cc)
				continue
			}
			if at, _ := v.aiaAt.Load(u); at != nil && time.Since(at.(time.Time)) < aiaRetry {
				continue
			}
		}
		v.aiaAt.Store(u, time.Now())
		cc := fetchIssuer(u)
		v.aia.Store(u, cc)
		if cc != nil {
			v.fetched.Add(1)
			out = append(out, cc)
		}
	}
	return out
}

func fetchIssuer(u string) *x509.Certificate {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "vtcseamless/"+Version+" (aia)")
	// Plain default transport: an issuer cert is self-describing and signed;
	// https AIA URLs just use the normal system roots.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil
	}
	if cs := ParsePEMCerts(b); len(cs) > 0 {
		return cs[0]
	}
	if c, err := x509.ParseCertificate(b); err == nil { // DER (what HARICA serves)
		return c
	}
	return nil
}

// TLSConfig is the client config for BEV requests.
func (v *Verifier) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Verification happens in VerifyConnection so extra intermediates
		// take part in chain building; roots and hostname are enforced there.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return v.Verify(cs.ServerName, cs.PeerCertificates)
		},
	}
}

// NewTransport clones http.DefaultTransport with the BEV trust config.
func NewTransport(caBundle string) (*http.Transport, error) {
	v, err := NewVerifier(caBundle)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = v.TLSConfig()
	return tr, nil
}

// defaultTransport is shared by Options.defaults (CLI / library use).
var defaultTransport = sync.OnceValue(func() *http.Transport {
	tr, err := NewTransport("")
	if err != nil { // unreadable BEVDIRECT_CA_BUNDLE: embedded + system only
		os.Unsetenv("BEVDIRECT_CA_BUNDLE")
		tr, _ = NewTransport("")
	}
	return tr
})

// ── upstream health ───────────────────────────────────────────────────

// Upstream tracks the outcome of tile fetches so /health can say whether
// BEV itself is reachable (vs. cells merely still assembling).
type Upstream struct {
	ok, fail, tlsErr, http5xx, otherErr atomic.Int64
	consecutive                         atomic.Int64
	lastOK, lastFail                    atomic.Int64 // unix nanos
	mu                                  sync.Mutex
	lastErr                             string
}

// UpstreamStatus is the /health → upstream document.
type UpstreamStatus struct {
	OK                  bool   `json:"ok"`
	State               string `json:"state"` // unknown | ok | degraded | down
	FetchOK             int64  `json:"fetch_ok"`
	FetchFailed         int64  `json:"fetch_failed"`
	TLSErrors           int64  `json:"tls_errors"`
	HTTP5xx             int64  `json:"http_5xx"`
	OtherErrors         int64  `json:"other_errors"`
	ConsecutiveFailures int64  `json:"consecutive_failures"`
	LastOKAt            string `json:"last_ok_at,omitempty"`
	LastErrorAt         string `json:"last_error_at,omitempty"`
	LastError           string `json:"last_error,omitempty"`
	Host                string `json:"host"`
}

type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("status %d", e.code) }

// ErrKind classifies a tile fetch error: "tls", "http_5xx" or "other".
func ErrKind(err error) string {
	var cve *tls.CertificateVerificationError
	var uae x509.UnknownAuthorityError
	var hne x509.HostnameError
	var cie x509.CertificateInvalidError
	var se *statusError
	switch {
	case errors.As(err, &cve), errors.As(err, &uae), errors.As(err, &hne), errors.As(err, &cie):
		return "tls"
	case errors.As(err, &se) && se.code >= 500:
		return "http_5xx"
	}
	if s := err.Error(); strings.Contains(s, "x509:") || strings.Contains(s, "tls:") {
		return "tls"
	}
	return "other"
}

func (u *Upstream) report(err error) {
	if u == nil {
		return
	}
	now := time.Now().UnixNano()
	if err == nil {
		u.ok.Add(1)
		u.consecutive.Store(0)
		u.lastOK.Store(now)
		return
	}
	u.fail.Add(1)
	u.consecutive.Add(1)
	u.lastFail.Store(now)
	switch ErrKind(err) {
	case "tls":
		u.tlsErr.Add(1)
	case "http_5xx":
		u.http5xx.Add(1)
	default:
		u.otherErr.Add(1)
	}
	u.mu.Lock()
	u.lastErr = err.Error()
	u.mu.Unlock()
}

// Status summarises the counters. down = ≥5 consecutive failures and no
// success for 2 min; degraded = the latest fetch failed; ok otherwise.
func (u *Upstream) Status() UpstreamStatus {
	st := UpstreamStatus{Host: tileHost(), FetchOK: u.ok.Load(), FetchFailed: u.fail.Load(), TLSErrors: u.tlsErr.Load(),
		HTTP5xx: u.http5xx.Load(), OtherErrors: u.otherErr.Load(), ConsecutiveFailures: u.consecutive.Load()}
	lo, lf := u.lastOK.Load(), u.lastFail.Load()
	if lo > 0 {
		st.LastOKAt = time.Unix(0, lo).UTC().Format(time.RFC3339)
	}
	if lf > 0 {
		st.LastErrorAt = time.Unix(0, lf).UTC().Format(time.RFC3339)
		u.mu.Lock()
		st.LastError = u.lastErr
		u.mu.Unlock()
	}
	switch {
	case lo == 0 && lf == 0:
		st.State, st.OK = "unknown", true
	case st.ConsecutiveFailures >= 5 && time.Since(time.Unix(0, lo)) > 2*time.Minute:
		st.State, st.OK = "down", false
	case lf > lo:
		st.State, st.OK = "degraded", true
	default:
		st.State, st.OK = "ok", true
	}
	return st
}

func tileHost() string {
	s := strings.TrimPrefix(TileURL, "https://")
	if i := strings.IndexByte(s, '/'); i > 0 {
		s = s[:i]
	}
	return s
}

func pemEncode(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
