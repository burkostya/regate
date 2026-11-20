package server

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type v1OAuthFixture struct {
	issuer, audience, jwks string
	private                *rsa.PrivateKey
}

func newV1OAuthFixture(t *testing.T, issuer, audience, jwks, realm string, private *rsa.PrivateKey) v1OAuthFixture {
	t.Helper()
	base := os.Getenv("REGATE_CONTROLLED_IDP_URL")
	if base == "" {
		return v1OAuthFixture{issuer: issuer, audience: audience, jwks: jwks, private: private}
	}
	target, err := url.Parse(base)
	if err != nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" ||
		target.Port() == "" || target.User != nil || target.Path != "" || target.RawQuery != "" || target.Fragment != "" {
		t.Fatal("controlled Keycloak must run on the test host's loopback HTTP port")
	}
	issuer = base + "/realms/" + realm
	return v1OAuthFixture{issuer: issuer, audience: "regate-local-acceptance", jwks: issuer + "/protocol/openid-connect/certs"}
}

func (f v1OAuthFixture) mint(t *testing.T, scopes, subject string) string {
	t.Helper()
	if f.private != nil {
		return signedTestJWTSubject(t, f.private, f.issuer, f.audience, scopes, subject)
	}
	secret := os.Getenv("REGATE_CONTROLLED_CLIENT_SECRET")
	if secret == "" {
		t.Fatal("controlled Keycloak service account credentials are missing")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scopes}}
	req, err := http.NewRequest(http.MethodPost, f.issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(subject, secret)
	client := &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&token) != nil || token.AccessToken == "" {
		t.Fatalf("Keycloak could not issue the integration token: HTTP %d", resp.StatusCode)
	}
	return token.AccessToken
}

func v1Ingress(t *testing.T, backend string) string {
	t.Helper()
	binary := os.Getenv("REGATE_CONTROLLED_NGINX")
	if binary == "" {
		return backend
	}
	cert, key := os.Getenv("REGATE_CONTROLLED_TLS_CERT"), os.Getenv("REGATE_CONTROLLED_TLS_KEY")
	if cert == "" || key == "" {
		t.Fatal("controlled ingress needs its disposable TLS key pair")
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	dir := t.TempDir()
	config := fmt.Sprintf(`
pid nginx.pid;
error_log stderr warn;
master_process off;
events { worker_connections 256; }
http {
    access_log off;
    server {
        listen %s ssl;
        ssl_certificate %q;
        ssl_certificate_key %q;
        ssl_protocols TLSv1.2 TLSv1.3;
        client_max_body_size 4m;
        location / {
            proxy_http_version 1.1;
            proxy_set_header Host $http_host;
            proxy_set_header Connection "";
            proxy_set_header Forwarded "";
            proxy_set_header X-Forwarded-For "";
            proxy_buffering off;
            proxy_request_buffering off;
            proxy_next_upstream off;
            proxy_read_timeout 90s;
            proxy_pass %s;
        }
    }
}
`, addr, cert, key, backend)
	path := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		reservation.Close()
		t.Fatal(err)
	}
	reservation.Close()
	cmd := exec.Command(binary, "-e", "stderr", "-p", dir, "-c", path, "-g", "daemon off;")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	base := "https://" + addr
	client := &http.Client{Timeout: 400 * time.Millisecond}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("TLS ingress did not reach the Regate health check")
	return ""
}
