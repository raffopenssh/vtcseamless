package bevdirect

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestEmbeddedIntermediateIsGEANT(t *testing.T) {
	cs := EmbeddedIntermediates()
	if len(cs) == 0 {
		t.Fatal("no embedded intermediates")
	}
	found := false
	for _, c := range cs {
		if c.Subject.CommonName == "GEANT TLS RSA 1" && c.IsCA && time.Now().Before(c.NotAfter) {
			found = true
		}
	}
	if !found {
		t.Fatal("GEANT TLS RSA 1 missing/expired in certs/")
	}
}

// TestVerifyRejectsUnknownChain: the custom VerifyConnection must still reject
// a self-signed server (we only ADD intermediates, never skip verification).
func TestVerifyRejectsUnknownChain(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	tr, err := NewTransport("")
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	_, err = c.Get(srv.URL)
	if err == nil {
		t.Fatal("self-signed server accepted")
	}
	var cve *tls.CertificateVerificationError
	if !errors.As(err, &cve) || ErrKind(err) != "tls" {
		t.Fatalf("want CertificateVerificationError/tls kind, got %T %v", err, err)
	}
}

// TestVerifyAcceptsViaBundle: the httptest CA passed as -ca-bundle makes the
// same server verify (hostname still checked).
func TestVerifyAcceptsViaBundle(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	leaf := srv.Certificate()
	f, _ := os.CreateTemp(t.TempDir(), "ca*.pem")
	f.Write(pemEncode(leaf.Raw))
	f.Close()
	tr, err := NewTransport(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := NewTransport(t.TempDir() + "/missing.pem"); err == nil {
		t.Fatal("missing bundle must error")
	}
}

func TestUpstreamStatus(t *testing.T) {
	var u Upstream
	if st := u.Status(); st.State != "unknown" || !st.OK {
		t.Fatalf("fresh: %+v", st)
	}
	u.report(nil)
	u.report(&statusError{503})
	st := u.Status()
	if st.State != "degraded" || st.HTTP5xx != 1 || st.ConsecutiveFailures != 1 {
		t.Fatalf("degraded: %+v", st)
	}
	u.lastOK.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	for i := 0; i < 5; i++ {
		u.report(x509.UnknownAuthorityError{})
	}
	st = u.Status()
	if st.State != "down" || st.OK || st.TLSErrors != 5 {
		t.Fatalf("down: %+v", st)
	}
	u.report(nil)
	if st := u.Status(); st.State != "ok" || st.ConsecutiveFailures != 0 {
		t.Fatalf("recovered: %+v", st)
	}
}

// TestLiveBEVChain talks to the real host when BEVDIRECT_LIVE=1: proves the
// embedded intermediate (or a fixed chain) verifies, and that the AIA fallback
// alone would too.
func TestLiveBEVChain(t *testing.T) {
	if os.Getenv("BEVDIRECT_LIVE") == "" {
		t.Skip("BEVDIRECT_LIVE unset")
	}
	for _, mode := range []string{"embedded", "aia-only"} {
		v, err := NewVerifier("")
		if err != nil {
			t.Fatal(err)
		}
		if mode == "aia-only" {
			v.inter = x509.NewCertPool()
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = v.TLSConfig()
		c := &http.Client{Transport: tr, Timeout: 15 * time.Second}
		resp, err := c.Get("https://" + tileHost() + "/")
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		resp.Body.Close()
		t.Logf("%s: status %d, aia fetched=%d", mode, resp.StatusCode, v.fetched.Load())
	}
}
