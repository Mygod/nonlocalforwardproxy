// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Caching is purposefully ignored.

package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mygod/nonlocalforwardproxy/httpclient"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"golang.org/x/net/proxy"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler implements a forward proxy.
//
// EXPERIMENTAL: This handler is still experimental and subject to breaking changes.
type Handler struct {
	logger *zap.Logger

	// Filename of the PAC file to serve.
	PACPath string `json:"pac_path,omitempty"`

	// If true, the Forwarded header will not be augmented with your IP address.
	HideIP bool `json:"hide_ip,omitempty"`

	// If true, the Via heaeder will not be added.
	HideVia bool `json:"hide_via,omitempty"`

	// Host(s) (and ports) of the proxy. When you configure a client,
	// you will give it the host (and port) of the proxy to use.
	Hosts caddyhttp.MatchHost `json:"hosts,omitempty"`

	// Optional probe resistance. (See documentation.)
	ProbeResistance *ProbeResistance `json:"probe_resistance,omitempty"`

	// How long to wait before timing out initial TCP connections.
	DialTimeout caddy.Duration `json:"dial_timeout,omitempty"`

	// Optionally configure an upstream proxy to use.
	Upstream string `json:"upstream,omitempty"`

	// Access control list.
	ACL []ACLRule `json:"acl,omitempty"`

	// Ports to be allowed to connect to (if non-empty).
	AllowedPorts []int `json:"allowed_ports,omitempty"`

	// Default address to send requests from if one is not specified from the proxy request
	DefaultBind *net.IPNet `json:"bind,omitempty"`

	//httpTransport *http.Transport

	// overridden dialContext allows us to redirect requests to upstream proxy
	dialContext func(ctx context.Context, network, address string, bind net.Addr) (net.Conn, error)
	upstream    *url.URL // address of upstream proxy
	randSource  *rand.Rand

	aclRules []aclRule

	// TODO: temporary/deprecated - we should try to reuse existing authentication modules instead!
	AuthRequired    bool     `json:"auth_required,omitempty"`
	AuthCredentials [][]byte `json:"auth_credentials,omitempty"` // slice with base64-encoded credentials
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.nonlocal_forward_proxy",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision ensures that h is set up properly before use.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)

	if h.DialTimeout <= 0 {
		h.DialTimeout = caddy.Duration(30 * time.Second)
	}

	// access control lists
	for _, rule := range h.ACL {
		for _, subj := range rule.Subjects {
			ar, err := newACLRule(subj, rule.Allow)
			if err != nil {
				return err
			}
			h.aclRules = append(h.aclRules, ar)
		}
	}
	for _, ipDeny := range []string{
		"10.0.0.0/8",
		"127.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"::1/128",
		"fe80::/10",
	} {
		ar, err := newACLRule(ipDeny, false)
		if err != nil {
			return err
		}
		h.aclRules = append(h.aclRules, ar)
	}
	h.aclRules = append(h.aclRules, &aclAllRule{allow: true})

	if h.DefaultBind != nil {
		h.randSource = rand.New(rand.NewSource(time.Now().UTC().UnixNano()))
	}

	if h.ProbeResistance != nil {
		if !h.AuthRequired {
			return fmt.Errorf("probe resistance requires authentication")
		}
		if len(h.ProbeResistance.Domain) > 0 {
			h.logger.Info("Secret domain used to connect to proxy: " + h.ProbeResistance.Domain)
		}
	}

	dialer := &net.Dialer{
		Timeout:   time.Duration(h.DialTimeout),
		KeepAlive: 30 * time.Second,
	}
	h.dialContext = func(ctx context.Context, network string, address string, bind net.Addr) (net.Conn, error) {
		if bind == nil {
			if h.DefaultBind != nil {
				ip := h.DefaultBind.IP
				ones, _ := h.DefaultBind.Mask.Size()
				h.randSource.Read(ip[ones>>3:])
				bind = &net.TCPAddr{IP: ip}
			}
			if bind == nil {
				return dialer.DialContext(ctx, network, address)
			}
		}
		d := *dialer // create a shallow copy
		d.ControlContext = freebindControlContext
		d.LocalAddr = bind
		return d.DialContext(ctx, bind.Network(), address)
	}

	if h.Upstream != "" {
		upstreamURL, err := url.Parse(h.Upstream)
		if err != nil {
			return fmt.Errorf("bad upstream URL: %v", err)
		}
		h.upstream = upstreamURL

		if !isLocalhost(h.upstream.Hostname()) && h.upstream.Scheme != "https" {
			return errors.New("insecure schemes are only allowed to localhost upstreams")
		}

		registerHTTPDialer := func(u *url.URL, _ proxy.Dialer) (proxy.Dialer, error) {
			// CONNECT request is proxied as-is, so we don't care about target url, but it could be
			// useful in future to implement policies of choosing between multiple upstream servers.
			// Given dialer is not used, since it's the same dialer provided by us.
			d, err := httpclient.NewHTTPConnectDialer(h.upstream.String())
			if err != nil {
				return nil, err
			}
			d.Dialer = *dialer
			if isLocalhost(h.upstream.Hostname()) && h.upstream.Scheme == "https" {
				// disabling verification helps with testing the package and setups
				// either way, it's impossible to have a legit TLS certificate for "127.0.0.1" - TODO: not true anymore
				h.logger.Info("Localhost upstream detected, disabling verification of TLS certificate")
				d.DialTLS = func(network string, address string) (net.Conn, string, error) {
					conn, err := tls.Dial(network, address, &tls.Config{InsecureSkipVerify: true})
					if err != nil {
						return nil, "", err
					}
					return conn, conn.ConnectionState().NegotiatedProtocol, nil
				}
			}
			return d, nil
		}
		proxy.RegisterDialerType("https", registerHTTPDialer)
		proxy.RegisterDialerType("http", registerHTTPDialer)

		upstreamDialer, err := proxy.FromURL(h.upstream, dialer)
		if err != nil {
			return errors.New("failed to create proxy to upstream: " + err.Error())
		}

		if ctxDialer, ok := upstreamDialer.(dialContexter); ok {
			// upstreamDialer has DialContext - use it
			h.dialContext = func(ctx context.Context, network string, address string, bind net.Addr) (net.Conn, error) {
				return ctxDialer.DialContext(ctx, network, address)
			}
		} else {
			// upstreamDialer does not have DialContext - ignore the context :(
			h.dialContext = func(ctx context.Context, network string, address string, bind net.Addr) (net.Conn, error) {
				return upstreamDialer.Dial(network, address)
			}
		}
	}

	return nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// start by splitting the request host and port
	reqHost, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		reqHost = r.Host // OK; probably just didn't have a port
	}

	var authErr error
	if h.AuthRequired {
		authErr = h.checkCredentials(r)
	}
	if h.ProbeResistance != nil && len(h.ProbeResistance.Domain) > 0 && reqHost == h.ProbeResistance.Domain {
		return serveHiddenPage(w, authErr)
	}
	if h.Hosts.Match(r) && (r.Method != http.MethodConnect || authErr != nil) {
		// Always pass non-CONNECT requests to hostname
		// Pass CONNECT requests only if probe resistance is enabled and not authenticated
		if h.shouldServePACFile(r) {
			return h.servePacFile(w, r)
		}
		return next.ServeHTTP(w, r)
	}
	if authErr != nil {
		if h.ProbeResistance != nil {
			// probe resistance is requested and requested URI does not match secret domain;
			// act like this proxy handler doesn't even exist (pass thru to next handler)
			return next.ServeHTTP(w, r)
		}
		w.Header().Set("Proxy-Authenticate", "Basic realm=\"Caddy Secure Web Proxy\"")
		return caddyhttp.Error(http.StatusProxyAuthRequired, authErr)
	}

	if r.ProtoMajor != 1 && r.ProtoMajor != 2 && r.ProtoMajor != 3 {
		return caddyhttp.Error(http.StatusHTTPVersionNotSupported,
			fmt.Errorf("unsupported HTTP major version: %d", r.ProtoMajor))
	}

	ctx := context.Background()
	if !h.HideIP {
		ctxHeader := make(http.Header)
		for k, v := range r.Header {
			if kL := strings.ToLower(k); kL == "forwarded" || kL == "x-forwarded-for" {
				ctxHeader[k] = v
			}
		}
		ctxHeader.Add("Forwarded", "for=\""+r.RemoteAddr+"\"")
		ctx = context.WithValue(ctx, httpclient.ContextKeyHeader{}, ctxHeader)
	}

	clientBind := r.Header.Get("Proxy-Nonlocal-Source")
	var bind net.Addr
	if len(clientBind) > 0 {
		if bind, err = net.ResolveTCPAddr("tcp", clientBind); err != nil {
			if ip, _ := net.ResolveIPAddr("ip", clientBind); ip != nil {
				bind = &net.TCPAddr{IP: ip.IP}
			}
		}
	}

	if r.Method == http.MethodConnect {
		if r.ProtoMajor == 2 || r.ProtoMajor == 3 {
			if len(r.URL.Scheme) > 0 || len(r.URL.Path) > 0 {
				return caddyhttp.Error(http.StatusBadRequest,
					fmt.Errorf("CONNECT request has :scheme and/or :path pseudo-header fields"))
			}
		}

		hostPort := r.URL.Host
		if hostPort == "" {
			hostPort = r.Host
		}
		targetConn, err := h.dialContextCheckACL(ctx, "tcp", hostPort, bind)
		if err != nil {
			return err
		}
		if targetConn == nil {
			// safest to check both error and targetConn afterwards, in case fp.dial (potentially unstable
			// from x/net/proxy) misbehaves and returns both nil or both non-nil
			return caddyhttp.Error(http.StatusForbidden,
				fmt.Errorf("hostname %s is not allowed", r.URL.Hostname()))
		}
		defer targetConn.Close()

		switch r.ProtoMajor {
		case 1: // http1: hijack the whole flow
			return serveHijack(w, targetConn)
		case 2: // http2: keep reading from "request" and writing into same response
			fallthrough
		case 3:
			defer r.Body.Close()
			wFlusher, ok := w.(http.Flusher)
			if !ok {
				return caddyhttp.Error(http.StatusInternalServerError,
					fmt.Errorf("ResponseWriter doesn't implement http.Flusher"))
			}
			w.WriteHeader(http.StatusOK)
			wFlusher.Flush()
			return dualStream(targetConn, r.Body, w)
		}

		panic("There was a check for http version, yet it's incorrect")
	}

	// Scheme has to be appended to avoid `unsupported protocol scheme ""` error.
	// `http://` is used, since this initial request itself is always HTTP, regardless of what client and server
	// may speak afterwards.
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = r.Host
	}
	r.Proto = "HTTP/1.1"
	r.ProtoMajor = 1
	r.ProtoMinor = 1
	r.RequestURI = ""

	removeHopByHop(r.Header)

	if !h.HideIP {
		r.Header.Add("Forwarded", "for=\""+r.RemoteAddr+"\"")
	}

	// https://tools.ietf.org/html/rfc7230#section-5.7.1
	if !h.HideVia {
		r.Header.Add("Via", strconv.Itoa(r.ProtoMajor)+"."+strconv.Itoa(r.ProtoMinor)+" caddy")
	}

	var response *http.Response
	if h.upstream == nil {
		// non-upstream request uses httpTransport to reuse connections
		if r.Body != nil &&
			(r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" || r.Method == "TRACE") {
			// make sure request is idempotent and could be retried by saving the Body
			// None of those methods are supposed to have body,
			// but we still need to copy the r.Body, even if it's empty
			rBodyBuf, err := ioutil.ReadAll(r.Body)
			if err != nil {
				return caddyhttp.Error(http.StatusBadRequest,
					fmt.Errorf("failed to read request body: %v", err))
			}
			r.GetBody = func() (io.ReadCloser, error) {
				return ioutil.NopCloser(bytes.NewReader(rBodyBuf)), nil
			}
			r.Body, _ = r.GetBody()
		}
		response, err = (&http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return h.dialContextCheckACL(ctx, network, address, bind)
			},
		}).RoundTrip(r)
	} else {
		// Upstream requests don't interact well with Transport: connections could always be
		// reused, but Transport thinks they go to different Hosts, so it spawns tons of
		// useless connections.
		// Just use dialContext, which will multiplex via single connection, if http/2
		if creds := h.upstream.User.String(); creds != "" {
			// set upstream credentials for the request, if needed
			r.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
		}
		if r.URL.Port() == "" {
			r.URL.Host = net.JoinHostPort(r.URL.Host, "80")
		}
		upsConn, err := h.dialContext(ctx, "tcp", r.URL.Host, bind)
		if err != nil {
			return caddyhttp.Error(http.StatusBadGateway,
				fmt.Errorf("failed to dial upstream: %v", err))
		}
		err = r.Write(upsConn)
		if err != nil {
			return caddyhttp.Error(http.StatusBadGateway,
				fmt.Errorf("failed to write upstream request: %v", err))
		}
		response, err = http.ReadResponse(bufio.NewReader(upsConn), r)
		if err != nil {
			return caddyhttp.Error(http.StatusBadGateway,
				fmt.Errorf("failed to read upstream response: %v", err))
		}
	}
	r.Body.Close()

	if response != nil {
		defer response.Body.Close()
	}
	if err != nil {
		//fmt.Printf("%v", err)
		if _, ok := err.(caddyhttp.HandlerError); ok {
			return err
		}
		return caddyhttp.Error(http.StatusBadGateway,
			fmt.Errorf("failed to read response: %v", err))
	}

	return forwardResponse(w, response)
}

func (h Handler) checkCredentials(r *http.Request) error {
	pa := strings.Split(r.Header.Get("Proxy-Authorization"), " ")
	if len(pa) != 2 {
		return errors.New("Proxy-Authorization is required! Expected format: <type> <credentials>")
	}
	if strings.ToLower(pa[0]) != "basic" {
		return errors.New("Auth type is not supported")
	}
	for _, creds := range h.AuthCredentials {
		if subtle.ConstantTimeCompare(creds, []byte(pa[1])) == 1 {
			repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
			buf := make([]byte, base64.StdEncoding.DecodedLen(len(creds)))
			_, _ = base64.StdEncoding.Decode(buf, creds) // should not err ever since we are decoding a known good input
			cred := string(buf)
			repl.Set("http.auth.user.id", cred[:strings.IndexByte(cred, ':')])
			// Please do not consider this to be timing-attack-safe code. Simple equality is almost
			// mindlessly substituted with constant time algo and there ARE known issues with this code,
			// e.g. size of smallest credentials is guessable. TODO: protect from all the attacks! Hash?
			return nil
		}
	}
	return errors.New("Invalid credentials")
}

func (h Handler) shouldServePACFile(r *http.Request) bool {
	return len(h.PACPath) > 0 && r.URL.Path == h.PACPath
}

func (h Handler) servePacFile(w http.ResponseWriter, r *http.Request) error {
	fmt.Fprintf(w, pacFile, r.Host)
	// fmt.Fprintf(w, pacFile, h.hostname, h.port)
	return nil
}

// dialContextCheckACL enforces Access Control List and calls fp.DialContext
func (h Handler) dialContextCheckACL(ctx context.Context, network, hostPort string, bind net.Addr) (net.Conn, error) {
	var conn net.Conn

	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		// return nil, &proxyError{S: "Network " + network + " is not supported", Code: http.StatusBadRequest}
		return nil, caddyhttp.Error(http.StatusBadRequest,
			fmt.Errorf("network %s is not supported", network))
	}

	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		// return nil, &proxyError{S: err.Error(), Code: http.StatusBadRequest}
		return nil, caddyhttp.Error(http.StatusBadRequest, err)
	}

	if h.upstream != nil {
		// if upstreaming -- do not resolve locally nor check acl
		conn, err = h.dialContext(ctx, network, hostPort, bind)
		if err != nil {
			// return conn, &proxyError{S: err.Error(), Code: http.StatusBadGateway}
			return conn, caddyhttp.Error(http.StatusBadGateway, err)
		}
		return conn, nil
	}

	if !h.portIsAllowed(port) {
		// return nil, &proxyError{S: "port " + port + " is not allowed", Code: http.StatusForbidden}
		return nil, caddyhttp.Error(http.StatusForbidden,
			fmt.Errorf("port %s is not allowed", port))
	}

	// in case IP was provided, net.LookupIP will simply return it
	IPs, err := net.LookupIP(host)
	if err != nil {
		// return nil, &proxyError{S: fmt.Sprintf("Lookup of %s failed: %v", host, err),
		// Code: http.StatusBadGateway}
		return nil, caddyhttp.Error(http.StatusBadGateway,
			fmt.Errorf("lookup of %s failed: %v", host, err))
	}

	// This is net.Dial's default behavior: if the host resolves to multiple IP addresses,
	// Dial will try each IP address in order until one succeeds
	err = nil
	for _, ip := range IPs {
		if !h.hostIsAllowed(host, ip) {
			continue
		}

		conn, err = h.dialContext(ctx, network, net.JoinHostPort(ip.String(), port), bind)
		if err == nil {
			return conn, nil
		}
	}
	if err != nil {
		return nil, caddyhttp.Error(http.StatusBadGateway, fmt.Errorf("dialContext: %v", err))
	}

	return nil, caddyhttp.Error(http.StatusForbidden, fmt.Errorf("no allowed IP addresses for %s", host))
}

func (h Handler) hostIsAllowed(hostname string, ip net.IP) bool {
	for _, rule := range h.aclRules {
		switch rule.tryMatch(ip, hostname) {
		case aclDecisionDeny:
			return false
		case aclDecisionAllow:
			return true
		}
	}
	// TODO: convert this to log entry
	// fmt.Println("ERROR: no acl match for ", hostname, ip) // shouldn't happen
	return false
}

func (h Handler) portIsAllowed(port string) bool {
	portInt, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	if portInt <= 0 || portInt > 65535 {
		return false
	}
	if len(h.AllowedPorts) == 0 {
		return true
	}
	isAllowed := false
	for _, p := range h.AllowedPorts {
		if p == portInt {
			isAllowed = true
			break
		}
	}
	return isAllowed
}

func serveHiddenPage(w http.ResponseWriter, authErr error) error {
	const hiddenPage = `<html>
<head>
  <title>Hidden Proxy Page</title>
</head>
<body>
<h1>Hidden Proxy Page!</h1>
%s<br/>
</body>
</html>`
	const AuthFail = "Please authenticate yourself to the proxy."
	const AuthOk = "Congratulations, you are successfully authenticated to the proxy! Go browse all the things!"

	w.Header().Set("Content-Type", "text/html")
	if authErr != nil {
		w.Header().Set("Proxy-Authenticate", "Basic realm=\"Caddy Secure Web Proxy\"")
		w.WriteHeader(http.StatusProxyAuthRequired)
		w.Write([]byte(fmt.Sprintf(hiddenPage, AuthFail)))
		return authErr
	}
	w.Write([]byte(fmt.Sprintf(hiddenPage, AuthOk)))
	return nil
}

// Hijacks the connection from ResponseWriter, writes the response and proxies data between targetConn
// and hijacked connection.
func serveHijack(w http.ResponseWriter, targetConn net.Conn) error {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return caddyhttp.Error(http.StatusInternalServerError,
			fmt.Errorf("ResponseWriter does not implement http.Hijacker"))
	}
	clientConn, bufReader, err := hijacker.Hijack()
	if err != nil {
		return caddyhttp.Error(http.StatusInternalServerError,
			fmt.Errorf("hijack failed: %v", err))
	}
	defer clientConn.Close()
	// bufReader may contain unprocessed buffered data from the client.
	if bufReader != nil {
		// snippet borrowed from `proxy` plugin
		if n := bufReader.Reader.Buffered(); n > 0 {
			rbuf, err := bufReader.Reader.Peek(n)
			if err != nil {
				return caddyhttp.Error(http.StatusBadGateway, err)
			}
			targetConn.Write(rbuf)
		}
	}
	// Since we hijacked the connection, we lost the ability to write and flush headers via w.
	// Let's handcraft the response and send it manually.
	res := &http.Response{StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	res.Header.Set("Server", "Caddy")

	err = res.Write(clientConn)
	if err != nil {
		return caddyhttp.Error(http.StatusInternalServerError,
			fmt.Errorf("failed to send response to client: %v", err))
	}

	return dualStream(targetConn, clientConn, clientConn)
}

// Copies data target->clientReader and clientWriter->target, and flushes as needed
// Returns when clientWriter-> target stream is done.
// Caddy should finish writing target -> clientReader.
func dualStream(target net.Conn, clientReader io.ReadCloser, clientWriter io.Writer) error {
	stream := func(w io.Writer, r io.Reader) error {
		// copy bytes from r to w
		buf := bufferPool.Get().([]byte)
		buf = buf[0:cap(buf)]
		_, _err := flushingIoCopy(w, r, buf)
		bufferPool.Put(buf)
		if cw, ok := w.(closeWriter); ok {
			cw.CloseWrite()
		}
		return _err
	}
	go stream(target, clientReader)
	return stream(clientWriter, target)
}

type closeWriter interface {
	CloseWrite() error
}

// flushingIoCopy is analogous to buffering io.Copy(), but also attempts to flush on each iteration.
// If dst does not implement http.Flusher(e.g. net.TCPConn), it will do a simple io.CopyBuffer().
// Reasoning: http2ResponseWriter will not flush on its own, so we have to do it manually.
func flushingIoCopy(dst io.Writer, src io.Reader, buf []byte) (written int64, err error) {
	flusher, ok := dst.(http.Flusher)
	if !ok {
		return io.CopyBuffer(dst, src, buf)
	}
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			flusher.Flush()
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return
}

// Removes hop-by-hop headers, and writes response into ResponseWriter.
func forwardResponse(w http.ResponseWriter, response *http.Response) error {
	w.Header().Del("Server") // remove Server: Caddy, append via instead
	w.Header().Add("Via", strconv.Itoa(response.ProtoMajor)+"."+strconv.Itoa(response.ProtoMinor)+" caddy")

	for header, values := range response.Header {
		for _, val := range values {
			w.Header().Add(header, val)
		}
	}
	removeHopByHop(w.Header())
	w.WriteHeader(response.StatusCode)
	buf := bufferPool.Get().([]byte)
	buf = buf[0:cap(buf)]
	_, err := io.CopyBuffer(w, response.Body, buf)
	bufferPool.Put(buf)
	return err
}

func removeHopByHop(header http.Header) {
	connectionHeaders := header.Get("Connection")
	for _, h := range strings.Split(connectionHeaders, ",") {
		header.Del(strings.TrimSpace(h))
	}
	for _, h := range hopByHopHeaders {
		header.Del(h)
	}
}

var hopByHopHeaders = []string{
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Upgrade",
	"Connection",
	"Proxy-Connection",
	"Proxy-Nonlocal-Source",
	"Te",
	"Trailer",
	"Transfer-Encoding",
}

const pacFile = `
function FindProxyForURL(url, host) {
	if (host === "127.0.0.1" || host === "::1" || host === "localhost")
		return "DIRECT";
	return "HTTPS %s";
}
`

var bufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 0, 32*1024)
	},
}

////// used during provision only

func isLocalhost(hostname string) bool {
	return hostname == "localhost" ||
		hostname == "127.0.0.1" ||
		hostname == "::1"
}

type dialContexter interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// ProbeResistance configures probe resistance.
type ProbeResistance struct {
	Domain string `json:"domain,omitempty"`
}

func readLinesFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var hostnames []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		hostnames = append(hostnames, scanner.Text())
	}

	return hostnames, scanner.Err()
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
