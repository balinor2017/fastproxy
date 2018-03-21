package superproxy

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/haxii/fastproxy/bufiopool"
)

func TestNewSuperProxy(t *testing.T) {
	go func() {
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		})
		http.ListenAndServe(":9999", nil)
	}()
	testNewSuperProxyWithHTTPSProxy(t)
	testNewSuperProxyWithHTTPType(t)
	testNewSuperProxyWithSocksType(t)
}
func testNewSuperProxyWithHTTPType(t *testing.T) {
	superProxy, err := NewSuperProxy("localhost", uint16(5080), ProxyTypeHTTP, "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if superProxy.GetProxyType() != ProxyTypeHTTP {
		t.Fatalf("unexpected proxy type")
	}
	if superProxy.HostWithPort() != "localhost:5080" {
		t.Fatalf("unexpected host with port")
	}
	if !bytes.Equal(superProxy.HostWithPortBytes(), []byte("localhost:5080")) {
		t.Fatalf("unexpected host with port bytes")
	}
	pool := bufiopool.New(1, 1)
	conn, err := superProxy.MakeTunnel(pool, "localhost:9999")
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost:9999\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	result := make([]byte, 1000)
	if _, err = conn.Read(result); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if !strings.Contains(string(result), "HTTP/1.1 200 OK") {
		t.Fatalf("unexpected result")
	}
}

func testNewSuperProxyWithSocksType(t *testing.T) {
	superProxy, err := NewSuperProxy("localhost", uint16(9099), ProxyTypeSOCKS5, "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if superProxy.GetProxyType() != ProxyTypeSOCKS5 {
		t.Fatalf("unexpected proxy type")
	}
	if superProxy.HostWithPort() != "localhost:9099" {
		t.Fatalf("unexpected host with port")
	}
	if !bytes.Equal(superProxy.HostWithPortBytes(), []byte("localhost:9099")) {
		t.Fatalf("unexpected host with port bytes")
	}

	pool := bufiopool.New(1, 1)
	conn, err := superProxy.MakeTunnel(pool, "localhost:9999")
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost:9999\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	result := make([]byte, 1000)
	if _, err = conn.Read(result); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if !strings.Contains(string(result), "HTTP/1.1 200 OK") {
		t.Fatalf("unexpected result")
	}
	defer conn.Close()
}

func testNewSuperProxyWithHTTPSProxy(t *testing.T) {
	superProxy, err := NewSuperProxy("localhost", uint16(443), ProxyTypeHTTPS, "", "", selfSignedCA)
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}
	if superProxy.GetProxyType() != ProxyTypeHTTPS {
		t.Fatalf("unexpected proxy type")
	}
	if superProxy.HostWithPort() != "localhost:443" {
		t.Fatalf("unexpected host with port")
	}
	if !bytes.Equal(superProxy.HostWithPortBytes(), []byte("localhost:443")) {
		t.Fatalf("unexpected host with port bytes")
	}
}
