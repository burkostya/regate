package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestValidateURL(t *testing.T) {
	t.Setenv("REGATE_ALLOWED_HOSTS", "api.example.com, second.example.com")
	for _, u := range []string{
		"http://127.0.0.1:9091/mcp", "http://localhost/mcp", "https://api.example.com/mcp", "https://second.example.com/mcp",
	} {
		if err := ValidateURL(u); err != nil {
			t.Errorf("valid URL %q: %v", u, err)
		}
	}
	for _, u := range []string{
		"file:///etc/passwd", "http://api.example.com/x", "https://evil.example.com",
		"https://169.254.169.254/latest/meta-data", "https://10.0.0.1/",
		"https://alice:password@api.example.com/",
		"http://localhost.evil.example.com/mcp",
		"http://127.0.0.2:80/",
	} {
		if err := ValidateURL(u); err == nil {
			t.Errorf("invalid URL %q was accepted", u)
		}
	}
}

func TestRejectDNSRebindingBeforeDial(t *testing.T) {
	t.Setenv("REGATE_ALLOWED_HOSTS", "api.example.com")
	called := false
	lookup := func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		called = true
		return nil, errors.New("should not be called")
	}
	_, err := guardedDial(context.Background(), "tcp", "api.example.com:443", lookup, dial)
	if err == nil || !strings.Contains(err.Error(), "blocked") || called {
		t.Fatalf("got err=%v dial-called=%v", err, called)
	}
}

func TestRejectMixedPublicAndPrivateDNS(t *testing.T) {
	lookup := func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("10.0.0.4")}}, nil
	}
	_, err := guardedDial(context.Background(), "tcp", "api.example.com:443", lookup, func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial should not happen for mixed DNS answers")
		return nil, nil
	})
	if err == nil {
		t.Fatal("mixed public/private IP answers were accepted")
	}
}

func TestHTTPClientPreservesLoopbackForLocalMocks(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })}
	defer srv.Close()
	go srv.Serve(listener)
	resp, err := Client(2 * time.Second).Get("http://" + listener.Addr().String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("unexpected HTTP %d", resp.StatusCode)
	}
}

func TestAllowlistDoesNotPermitPrivateIP(t *testing.T) {
	os.Setenv("REGATE_ALLOWED_HOSTS", "internal.example.com")
	defer os.Unsetenv("REGATE_ALLOWED_HOSTS")
	if permittedAddress("internal.example.com", net.ParseIP("192.168.1.1")) {
		t.Fatal("private IP allowed")
	}
	if permittedAddress("internal.example.com", net.ParseIP("127.0.0.1")) {
		t.Fatal("loopback IP allowed")
	}
	if !permittedAddress("internal.example.com", net.ParseIP("1.1.1.1")) {
		t.Fatal("public IP incorrectly blocked")
	}
}
