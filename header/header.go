package header

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/haxii/fastproxy/bytebufferpool"
)

//Header header part of http request & respose
type Header struct {
	isConnectionClose bool
	contentLength     int64
}

//Reset reset header info into default val
func (header *Header) Reset() {
	header.isConnectionClose = false
	header.contentLength = 0
}

//IsConnectionClose is connection header set to `close`
func (header *Header) IsConnectionClose() bool {
	return header.isConnectionClose
}

//ContentLength content length header value,
func (header *Header) ContentLength() int64 {
	if header.contentLength > 0 {
		return header.contentLength
	}
	return 0
}

//IsBodyChunked if body is set `chunked`
func (header *Header) IsBodyChunked() bool {
	// negative means transfer encoding: -1 means chunked;  -2 means identity
	return header.contentLength == -1
}

//IsBodyIdentity if body is set `identity`
func (header *Header) IsBodyIdentity() bool {
	// negative means transfer encoding: -1 means chunked;  -2 means identity
	return header.contentLength == -2
}

// ParseHeaderFields parse http header fields from reader and write it into buffer,
//
// Each header field consists of a case-insensitive field name followed
// by a colon (":"), optional leading whitespace, the field value, and
// optional trailing whitespace.
func (header *Header) ParseHeaderFields(reader *bufio.Reader,
	buffer *bytebufferpool.ByteBuffer) error {
	n := 1
	for {
		err := header.tryRead(reader, buffer, n)
		if err == nil {
			return nil
		}
		if err != errNeedMore {
			buffer.Reset()
			return err
		}
		n = reader.Buffered() + 1
	}
}

var errNeedMore = errors.New("need more data: cannot find trailing lf")

func (header *Header) tryRead(reader *bufio.Reader,
	buffer *bytebufferpool.ByteBuffer, n int) error {
	errWrapper := func(err error) error {
		return fmt.Errorf("error when reading http headers: %s", err)
	}
	//do NOT use reader.ReadBytes here
	//which would allocate extra byte memory
	if b, err := reader.Peek(n); len(b) == 0 {
		// treat all errors on the first byte read as EOF
		if n == 1 || err == io.EOF {
			return fmt.Errorf("error when reading http headers: %s", io.EOF)
		}
		return errWrapper(err)
	}
	//must read buffed bytes
	b, err := reader.Peek(reader.Buffered())
	if len(b) == 0 || err != nil {
		panic(fmt.Sprintf("bufio.Reader.Peek() returned unexpected data (%q, %v)", b, err))
	}
	//try to read it into buffer
	headersLen, errParse := header.readHeaders(b, buffer)
	if errParse != nil {
		if errParse == errNeedMore {
			return errNeedMore
		}
		return errWrapper(errParse)
	}
	//jump over the header fields
	if _, err := reader.Discard(headersLen); err != nil {
		panic(fmt.Sprintf("bufio.Reader.Discard(%d) failed: %s", headersLen, err))
	}
	return nil
}

func (header *Header) readHeaders(buf []byte,
	buffer *bytebufferpool.ByteBuffer) (_headerLength int, _err error) {
	parseThenWriteBuffer := func(rawHeaderLine []byte) error {
		// Connection, Authenticate and Authorization are single hop Header:
		// http://www.w3.org/Protocols/rfc2616/rfc2616.txt
		// 14.10 Connection
		//   The Connection general-header field allows the sender to specify
		//   options that are desired for that particular connection and MUST NOT
		//   be communicated by proxies over further connections.
		if isConnectionHeader(rawHeaderLine) {
			changeToLowerCase(rawHeaderLine)
			if bytes.Contains(rawHeaderLine, []byte("close")) {
				header.isConnectionClose = true
			}
			return nil
		}

		// parse content length
		// content length > 0 means the length of the body
		// content length < 0 means the transfer encoding is set,
		//  -1 means chunked
		//  -2 means identity
		if isContentLengthHeader(rawHeaderLine) {
			lengthBytesIndex := bytes.IndexByte(rawHeaderLine, ':')
			if lengthBytesIndex > 0 {
				lengthBytes := rawHeaderLine[lengthBytesIndex+1:]
				length, _ := strconv.ParseInt(strings.TrimSpace(string(lengthBytes)), 10, 64)
				if length > 0 {
					header.contentLength = length
				}
			}
		} else if isTransferEncodingHeader(rawHeaderLine) {
			if bytes.Contains(rawHeaderLine, []byte("chunked")) {
				header.contentLength = -1
			} else if bytes.Contains(rawHeaderLine, []byte("identity")) {
				header.contentLength = -2
			}
		}

		//remove proxy header
		if !isProxyHeader(rawHeaderLine) {
			if _, e := buffer.Write(rawHeaderLine); e != nil {
				return e
			}
		}
		return nil
	}

	//read 1st line
	n := bytes.IndexByte(buf, '\n')
	if n < 0 {
		return 0, errNeedMore
	}
	if (n == 1 && buf[0] == '\r') || n == 0 {
		// empty headers
		return n + 1, nil
	}
	n++
	buffer.Reset()
	if e := parseThenWriteBuffer(buf[:n]); e != nil {
		buffer.Reset()
		return 0, e
	}

	//read rest lines
	b := buf
	m := n
	for {
		b = b[m:]
		m = bytes.IndexByte(b, '\n')
		if m < 0 {
			buffer.Reset()
			return 0, errNeedMore
		}
		m++
		if e := parseThenWriteBuffer(b[:m]); e != nil {
			buffer.Reset()
			return 0, e
		}
		n += m
		if (m == 2 && b[0] == '\r') || m == 1 {
			return n, nil
		}
	}
}

var proxyHeaders = [][]byte{
	// If no Accept-Encoding header exists, Transport will add the headers it can accept
	// and would wrap the response body with the relevant reader.
	[]byte("Accept-Encoding"),
	// curl can add that, see
	// https://jdebp.eu./FGA/web-proxy-connection-header.html
	[]byte("Proxy-Connection"),
	[]byte("Proxy-Authenticate"),
	[]byte("Proxy-Authorization"),
}

func isProxyHeader(header []byte) bool {
	for _, proxyHeaderKey := range proxyHeaders {
		if bytes.HasPrefix(header, proxyHeaderKey) {
			return true
		}
	}
	return false
}

var connectionHeader = []byte("Connection")

func isConnectionHeader(header []byte) bool {
	return bytes.HasPrefix(header, connectionHeader)
}

var contentLengthHeader = []byte("Content-Length")

func isContentLengthHeader(header []byte) bool {
	return bytes.HasPrefix(header, contentLengthHeader)
}

var transferEncoding = []byte("Transfer-Encoding")

func isTransferEncodingHeader(header []byte) bool {
	return bytes.HasPrefix(header, transferEncoding)

}