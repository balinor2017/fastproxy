package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/haxii/fastproxy/bufiopool"
	"github.com/haxii/fastproxy/cert"
	"github.com/haxii/fastproxy/client"
	"github.com/haxii/fastproxy/transport"
)

//handler proxy http & https handler
type handler struct {
	// CA specifies the root CA for generating leaf certs for
	// each incoming TLS request.
	CA *tls.Certificate

	// buffer reader and writer pool
	bufioPool *bufiopool.Pool

	// sniffer https sniffer
	sniffer Sniffer
}

func (h *handler) sendHTTPSProxyStatusOK(c net.Conn) (err error) {
	_, err = c.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	return
}

func (h *handler) sendHTTPSProxyStatusBadGateway(c net.Conn) (err error) {
	_, err = c.Write([]byte("HTTP/1.1 501 Bad Gateway\r\n\r\n"))
	return
}

//proxy https traffic directly
func (h *handler) tunnelConnect(c *net.Conn, host string) error {
	errorWrapper := func(msg string, err error) error {
		return fmt.Errorf("%s: %s", msg, err)
	}
	//acquire server conn to target host
	var targetConn *net.Conn
	if _targetConn, err := transport.Dial(host); err == nil {
		targetConn = &_targetConn
	} else {
		h.sendHTTPSProxyStatusBadGateway(*c)
		return errorWrapper("error occurred when dialing to host"+host, err)
	}
	defer (*targetConn).Close()

	//handshake with client
	if err := h.sendHTTPSProxyStatusOK(*c); err != nil {
		return errorWrapper("error occurred when handshaking with client", err)
	}
	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func(e error) {
		err1 = transport.Forward(*targetConn, *c)
		wg.Done()
	}(err1)
	go func(e error) {
		err2 = transport.Forward(*c, *targetConn)
		wg.Done()
	}(err2)
	wg.Wait()
	if err1 != nil {
		return errorWrapper("error occurred when tunneling client request to client", err1)
	}
	if err2 != nil {
		return errorWrapper("error occurred when tunneling client response to client", err2)
	}
	return nil
}

//proxy the https connetions by MITM
func (h *handler) decryptConnect(c net.Conn, client *client.Client, hostWithPort string) error {
	errorWrapper := func(msg string, err error) error {
		return fmt.Errorf("%s: %s", msg, err)
	}
	//fakeTargetServer means a fake target server for remote client
	//make a connection with client by creating a fake target server
	//
	//make a fake target server's certificate
	fakeTargetServerCert, err := h.signFakeCert(hostWithPort)
	if err != nil {
		h.sendHTTPSProxyStatusBadGateway(c)
		return errorWrapper("error occurred when signing fake certificate for client", err)
	}
	//make the target server's config with this fake certificate
	targetServerName := ""
	fakeTargetServerTLSConfig := &tls.Config{
		Certificates: []tls.Certificate{*fakeTargetServerCert},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			targetServerName = hello.ServerName
			return cert.GenCert(h.CA, []string{hello.ServerName})
		},
	}
	//perform the proxy hand shake and fake tls handshake
	handShake := func() (*tls.Conn, error) {
		//make the proxy handshake
		if err := h.sendHTTPSProxyStatusOK(c); err != nil {
			return nil, fmt.Errorf("proxy handshaking error: %s", err)
		}
		//make the tls handshake in https
		conn := tls.Server(c, fakeTargetServerTLSConfig)
		if err := conn.Handshake(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("fake server tls handshaking error: %s", err)
		}
		return conn, nil
	}
	fakeServerConn, err := handShake()
	if len(targetServerName) == 0 {
		err = errors.New("client didn't provide a target server name")
	}
	if err != nil {
		return errorWrapper("error occurred when handshaking with client", err)
	}
	defer fakeServerConn.Close()

	//make a connection with target server by creating a fake remote client
	//
	//convert fakeServerConn into a http request
	reader := h.bufioPool.AcquireReader(fakeServerConn)
	defer h.bufioPool.ReleaseReader(reader)
	req := AcquireRequest()
	defer ReleaseRequest(req)
	if err := req.InitWithTLSClientReader(reader, h.sniffer, hostWithPort, targetServerName); err != nil {
		return errorWrapper("fail to read MITMed https request header", err)
	}
	//convert fakeServerConn into a http response
	writer := h.bufioPool.AcquireWriter(fakeServerConn)
	defer h.bufioPool.ReleaseWriter(writer)
	defer writer.Flush()
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	if err := resp.InitWithWriter(writer, h.sniffer); err != nil {
		return errorWrapper("fail to init MITMed https response header", err)
	}
	//handle fake https client request
	if e := client.Do(req, resp); e != nil {
		return errorWrapper("fail to make MITMed https client request ", e)
	}
	return nil
}

func (h *handler) signFakeCert(host string) (*tls.Certificate, error) {
	domain, _, err := net.SplitHostPort(host)
	if err != nil {
		return nil, fmt.Errorf("get host's %s domain with error %s", host, err)
	}
	cert, err2 := cert.GenCert(h.CA, []string{domain})
	if err2 != nil {
		return nil, fmt.Errorf("sign %s fake cert with error %s", domain, err2)
	}
	return cert, nil
}