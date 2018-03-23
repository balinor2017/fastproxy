package proxy

import (
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/haxii/fastproxy/bufiopool"
	"github.com/haxii/fastproxy/cert"
	"github.com/haxii/fastproxy/client"
	"github.com/haxii/fastproxy/hijack"
	"github.com/haxii/fastproxy/proxy/http"
	"github.com/haxii/fastproxy/superproxy"
	"github.com/haxii/fastproxy/transport"
	"github.com/haxii/fastproxy/usage"
	"github.com/haxii/fastproxy/util"
)

var (
	//ErrSessionUnavailable means that proxy can't serve this session
	ErrSessionUnavailable = errors.New("session unavailable")
)

//Handler proxy handler
type Handler struct {
	//ShouldAllowConnection should allow the connection to proxy, return false to drop the conn
	ShouldAllowConnection func(connAddr net.Addr) bool

	//HTTPSDecryptEnable test if host's https connection should be decrypted
	ShouldDecryptHost func(host string) bool

	//URLProxy url specified proxy, nil path means this is a un-decrypted https traffic
	URLProxy func(hostInfo *http.HostInfo, path []byte) *superproxy.SuperProxy

	//LookupIP returns ip string,
	//should not block for long time
	LookupIP func(domain string) net.IP

	//hijacker pool for making a hijacker for every incoming request
	HijackerPool hijack.HijackerPool
	//hijacker client for make hijacked response if available
	hijackClient hijack.Client
	//MitmCACert HTTPSDecryptCACert ca.cer used for https decryption
	MitmCACert *tls.Certificate

	//http requests and response pool
	reqPool  http.RequestPool
	respPool http.ResponsePool
}

func (h *Handler) handleHTTPConns(c net.Conn, req *http.Request,
	bufioPool *bufiopool.Pool, client *client.Client, usage *usage.ProxyUsage) error {
	return h.do(c, req, bufioPool, client, usage)
}

func (h *Handler) do(c net.Conn, req *http.Request,
	bufioPool *bufiopool.Pool, client *client.Client, usage *usage.ProxyUsage) error {
	//convert connetion into a http response
	writer := bufioPool.AcquireWriter(c)
	defer bufioPool.ReleaseWriter(writer)
	defer writer.Flush()
	resp := h.respPool.Acquire()
	defer h.respPool.Release(resp)
	if err := resp.WriteTo(writer); err != nil {
		return err
	}

	//set requests hijacker
	hijacker := h.HijackerPool.Get(c.RemoteAddr(),
		req.HostInfo().HostWithPort(), req.Method(), req.PathWithQueryFragment())
	defer h.HijackerPool.Put(hijacker)

	//set request & response hijacker
	req.SetHijacker(hijacker)
	resp.SetHijacker(hijacker)
	if hijackedRespReader := hijacker.HijackResponse(); hijackedRespReader != nil {
		reqReadSize, _, respSize, err := h.hijackClient.Do(req, resp, hijackedRespReader)
		go func() {
			client.Usage.AddIncomingSize(uint64(reqReadSize))
			client.Usage.AddOutgoingSize(uint64(respSize))
		}()

		return err
	}

	//set requests proxy
	superProxy := h.URLProxy(req.HostInfo(), req.PathWithQueryFragment())
	if len(req.HostInfo().HostWithPort()) == 0 {
		return ErrSessionUnavailable
	}

	req.SetProxy(superProxy)
	if superProxy != nil {
		domain := req.HostInfo().Domain()
		if len(domain) > 0 {
			ip := h.lookupIP(domain)
			req.HostInfo().SetIP(ip)
		}

		superProxy.AcquireToken()
		defer func() {
			superProxy.PushBackToken()
		}()
	}

	//handle http proxy request
	reqReadNum, reqWriteNum, respNum, err := client.Do(req, resp)

	go func() {
		client.Usage.AddIncomingSize(uint64(reqReadNum))
		client.Usage.AddOutgoingSize(uint64(respNum))
		if superProxy != nil {
			superProxy.Usage.AddIncomingSize(uint64(respNum))
			superProxy.Usage.AddOutgoingSize(uint64(reqWriteNum))
		}
	}()

	return err
}

func (h *Handler) handleHTTPSConns(c net.Conn, hostWithPort string,
	bufioPool *bufiopool.Pool, client *client.Client, usage *usage.ProxyUsage, idle time.Duration) error {
	if h.ShouldDecryptHost(hostWithPort) {
		return h.decryptConnect(c, hostWithPort, bufioPool, client, usage)
	}
	return h.tunnelConnect(c, bufioPool, hostWithPort, usage, idle)
}

const (
	httpTunnelMadeOk    = "HTTP/1.1 200 OK\r\n\r\n"
	httpTunnelMadeError = "HTTP/1.1 501 Bad Gateway\r\n\r\n"
)

var (
	httpTunnelMadeOkBytes    = []byte(httpTunnelMadeOk)
	httpTunnelMadeErrorBytes = []byte(httpTunnelMadeError)

	httpTunnelMadeOkSize    = uint64(len(httpTunnelMadeOkBytes))
	httpTunnelMadeErrorSize = uint64(len(httpTunnelMadeErrorBytes))
)

func (h *Handler) sendHTTPSProxyStatusOK(c net.Conn) (err error) {
	_, err = util.WriteWithValidation(c, httpTunnelMadeOkBytes)
	return err
}

func (h *Handler) sendHTTPSProxyStatusBadGateway(c net.Conn) (err error) {
	_, err = util.WriteWithValidation(c, httpTunnelMadeErrorBytes)
	return err
}

//proxy https traffic directly
func (h *Handler) tunnelConnect(conn net.Conn, bufioPool *bufiopool.Pool,
	hostWithPort string, usage *usage.ProxyUsage, idle time.Duration) error {
	hostInfo := &http.HostInfo{}
	hostInfo.ParseHostWithPort(hostWithPort)
	superProxy := h.URLProxy(hostInfo, nil)
	if len(hostInfo.HostWithPort()) == 0 {
		return ErrSessionUnavailable
	}
	hostWithPort = hostInfo.HostWithPort()
	targetWithPort := hostWithPort

	if superProxy != nil {
		host, port, _ := net.SplitHostPort(hostWithPort)
		if len(host) > 0 && net.ParseIP(host) == nil {
			ip := h.lookupIP(host)
			if ip != nil {
				targetWithPort = ip.String() + ":" + port
			}
		}
		//limit concurrency
		superProxy.AcquireToken()
		defer func() {
			superProxy.PushBackToken()
		}()
	}

	var (
		tunnelConn net.Conn
		err        error
	)
	if superProxy != nil {
		//acquire server conn to target host
		tunnelConn, err = superProxy.MakeTunnel(bufioPool, targetWithPort)
	} else {
		//acquire server conn to target host
		tunnelConn, err = transport.Dial(hostWithPort)
	}

	var proxyIncomingSize, proxyOutgoingSize, superProxyIncomingSize, superProxyOutgoingSize uint64
	defer func() {
		go func() {
			if usage != nil {
				usage.AddIncomingSize(proxyIncomingSize)
				usage.AddOutgoingSize(proxyOutgoingSize)
			}
			if superProxy != nil {
				superProxy.Usage.AddOutgoingSize(superProxyOutgoingSize)
				superProxy.Usage.AddIncomingSize(superProxyIncomingSize)
			}
		}()
	}()

	if err != nil {
		h.sendHTTPSProxyStatusBadGateway(conn)
		proxyOutgoingSize += httpTunnelMadeErrorSize
		return util.ErrWrapper(err, "error occurred when dialing to host "+hostWithPort)
	}
	defer tunnelConn.Close()

	//handshake with client
	if err := h.sendHTTPSProxyStatusOK(conn); err != nil {
		return util.ErrWrapper(err, "error occurred when handshaking with client")
	}
	proxyOutgoingSize += httpTunnelMadeOkSize

	var wg sync.WaitGroup
	var superProxyWriteErr, superProxyReadErr error
	var superProxyOutgoingTrafficSize, superProxyIncomingTrafficSize int64
	wg.Add(2)
	go func() {
		superProxyOutgoingTrafficSize, superProxyWriteErr = transport.Forward(tunnelConn, conn, idle)
		wg.Done()
	}()
	go func() {
		superProxyIncomingTrafficSize, superProxyReadErr = transport.Forward(conn, tunnelConn, idle)
		wg.Done()
	}()
	wg.Wait()

	proxyIncomingSize += uint64(superProxyOutgoingTrafficSize)
	proxyOutgoingSize += uint64(superProxyIncomingTrafficSize)
	superProxyIncomingSize += uint64(superProxyIncomingTrafficSize)
	superProxyOutgoingSize += uint64(superProxyOutgoingTrafficSize)

	if superProxyWriteErr != nil {
		return util.ErrWrapper(superProxyWriteErr, "error occurred when tunneling client request to client")
	}
	if superProxyReadErr != nil {
		return util.ErrWrapper(superProxyReadErr, "error occurred when tunneling client response to client")
	}
	return nil
}

//proxy the https connetions by MITM
func (h *Handler) decryptConnect(c net.Conn, hostWithPort string,
	bufioPool *bufiopool.Pool, client *client.Client, usage *usage.ProxyUsage) error {
	//fakeTargetServer means a fake target server for remote client
	//make a connection with client by creating a fake target server
	//
	//make a fake target server's certificate
	fakeTargetServerCert, err := h.signFakeCert(h.MitmCACert, hostWithPort)
	if err != nil {
		h.sendHTTPSProxyStatusBadGateway(c)
		if usage != nil {
			go func() {
				usage.AddOutgoingSize(httpTunnelMadeErrorSize)
			}()
		}
		return util.ErrWrapper(err, "fail to sign fake certificate for client")
	}
	//make the target server's config with this fake certificate
	targetServerName := ""
	fakeTargetServerTLSConfig := &tls.Config{
		Certificates: []tls.Certificate{*fakeTargetServerCert},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			targetServerName = hello.ServerName
			return cert.GenCert(h.MitmCACert, []string{hello.ServerName})
		},
	}
	//perform the proxy hand shake and fake tls handshake
	handShake := func() (*tls.Conn, error) {
		//make the proxy handshake
		if err := h.sendHTTPSProxyStatusOK(c); err != nil {
			return nil, util.ErrWrapper(err, "proxy fails to handshake with client")
		}
		if usage != nil {
			go func() {
				usage.AddOutgoingSize(httpTunnelMadeOkSize)
			}()
		}
		//make the tls handshake in https
		conn := tls.Server(c, fakeTargetServerTLSConfig)
		if err := conn.Handshake(); err != nil {
			conn.Close()
			return nil, util.ErrWrapper(err, "fake tls server fails to handshake with client")
		}
		return conn, nil
	}
	fakeServerConn, err := handShake()
	if len(targetServerName) == 0 {
		err = errors.New("client didn't provide a target server name")
	}
	if err != nil {
		return err
	}
	defer fakeServerConn.Close()

	//make a connection with target server by creating a fake remote client
	//
	//convert fakeServerConn into a http request
	reader := bufioPool.AcquireReader(fakeServerConn)
	defer bufioPool.ReleaseReader(reader)
	req := h.reqPool.Acquire()
	defer h.reqPool.Release(req)
	rn, err := req.ReadFrom(reader)
	if err != nil {
		return util.ErrWrapper(err, "fail to read fake tls server request header")
	}

	if usage != nil {
		go func() {
			usage.AddIncomingSize(uint64(rn))
		}()
	}

	req.SetTLS(targetServerName)
	//mandatory for tls request cause non hosts provided in request header
	req.SetHostWithPort(hostWithPort)

	return h.do(fakeServerConn, req, bufioPool, client, usage)
}

func (h *Handler) signFakeCert(mitmCACert *tls.Certificate, host string) (*tls.Certificate, error) {
	domain, _, err := net.SplitHostPort(host)
	if err != nil {
		return nil, err
	}
	cert, err2 := cert.GenCert(mitmCACert, []string{domain})
	if err2 != nil {
		return nil, err2
	}
	return cert, nil
}

//resolve domain to ip
func (h *Handler) lookupIP(domain string) net.IP {
	if h.LookupIP == nil {
		return nil
	}
	return h.LookupIP(domain)
}
