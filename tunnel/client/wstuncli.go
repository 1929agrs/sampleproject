// Copyright (c) 2014 RightScale, Inc. - see LICENSE

// Websockets tunnel client, which runs at the HTTP server end (yes, I know, it's confusing)
// This client connects to a websockets tunnel server and waits to receive HTTP requests
// tunneled through the websocket, then issues these HTTP requests locally to an HTTP server
// grabs the response and ships that back through the tunnel.
//
// This client is highly concurrent: it spawns a goroutine for each received request and issues
// that concurrently to the HTTP server and then sends the response back whenever the HTTP
// request returns. The response can thus go back out of order and multiple HTTP requests can
// be in flight at a time.
//
// This client also sends periodic ping messages through the websocket and expects prompt
// responses. If no response is received, it closes the websocket and opens a new one.
//
// The main limitation of this client is that responses have to go throught the same socket
// that the requests arrived on. Thus, if the websocket dies while an HTTP request is in progress
// it impossible for the response to travel on the next websocket, instead it will be dropped
// on the floor. This should not be difficult to fix, though.
//
// Another limitation is that it keeps a single websocket open and can thus get stuck for
// many seconds until the timeout on the websocket hits and a new one is opened.

package client

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"os"
	"regexp"
	"runtime"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	_ "net/http/pprof"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/inconshreveable/log15.v2"
	"gofrugal/wstunnel/tunnel/util"
)

var _ fmt.Formatter

// WSTunnelClient represents a persistent tunnel that can cycle through many websockets. The
// fields in this struct are relatively static/constant. The conn field points to the latest
// websocket, but it's important to realize that there may be goroutines handling older
// websockets that are not fully closed yet running at any point in time
type WSTunnelClient struct {
	Token          string         // Rendez-vous token
	OrderNumber    string         // Order Number
	Tunnel         string         // websocket server to connect to (ws[s]://hostname:port)
	Server         string         // local HTTP(S) server to send received requests to (default server)
	InternalServer http.Handler   // internal Server to dispatch HTTP requests to
	Regexp         *regexp.Regexp // regexp for allowed local HTTP(S) servers
	Insecure       bool           // accept self-signed SSL certs from local HTTPS servers
	Timeout        time.Duration  // timeout on websocket
	Proxy          *url.URL       // if non-nil, external proxy to use
	StatusFd       *os.File       // output periodic tunnel status information
	Connected      bool           // true when we have an active connection to wstunsrv
	exitChan       chan struct{}  // channel to tell the tunnel goroutines to end
	conn           *WSConnection
}

// WSConnection represents a single websocket connection
type WSConnection struct {
	ws  *websocket.Conn // websocket connection
	tun *WSTunnelClient // link back to tunnel
}

// Tunnel Client Arg
type TunnelClientArg struct {
	Token      string // token
	OrderNo    string // order number
	TunnelUrl  string // tunnel url
	ServerPath string // server-url to the request routed (eg: http://localhost:8482)
}

var httpClient http.Client = http.Client{
	// https://golang.org/pkg/net/http/#Client.CheckRedirect
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
} // client used for all requests, gets special transport for -insecure

//===== Main =====

func NewWSTunnelClient(clientArg *TunnelClientArg) *WSTunnelClient {

	wstunCli := WSTunnelClient{}

	// rendez-vous token identifying this server
	wstunCli.Token = clientArg.Token

	// Order number
	wstunCli.OrderNumber = clientArg.OrderNo

	// websocket server ws[s]://hostname:port to connect to
	var tunnel string = clientArg.TunnelUrl
	wstunCli.Tunnel = tunnel

	// http server http[s]://hostname:port to send received requests to
	wstunCli.Server = clientArg.ServerPath

	// accept self-signed SSL certs from local HTTPS servers
	var insecure bool = false
	wstunCli.Insecure = insecure

	var sre string = ""
	var tout int = 30
	var pidf string = ""
	// var logf string = ""
	var statf string = ""
	var proxy string = ""

	helpers.WritePid(pidf)
	wstunCli.Timeout = helpers.CalcWsTimeout(tout)

	// process -statusfile
	if statf != "" {
		fd, err := os.Create(statf)
		if err != nil {
			log15.Crit("Can't create statusfile", "err", err.Error())
			os.Exit(1)
		}
		wstunCli.StatusFd = fd
	}

	// process -regexp
	if sre != "" {
		var err error
		wstunCli.Regexp, err = regexp.Compile(sre)
		if err != nil {
			log15.Crit("Can't parse -regexp", "err", err.Error())
			os.Exit(1)
		}
	}

	// process -proxy or look for standard unix env variables
	if proxy == "" {
		envNames := []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}
		for _, n := range envNames {
			if p := os.Getenv(n); p != "" {
				proxy = p
				break
			}
		}
	}
	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil || !strings.HasPrefix(proxyURL.Scheme, "http") {
			// proxy was bogus. Try prepending "http://" to it and
			// see if that parses correctly. If not, we fall
			// through and complain about the original one.
			if proxyURL, err = url.Parse("http://" + proxy); err != nil {
				log15.Crit(fmt.Sprintf("Invalid proxy address: %q, %v", proxy, err))
				os.Exit(1)
			}
		}

		wstunCli.Proxy = proxyURL
	}

	return &wstunCli
}

func (t *WSTunnelClient) Start() error {
	log15.Info(helpers.VV)

	// validate -tunnel
	if t.Tunnel == "" {
		return fmt.Errorf("Must specify tunnel server ws://hostname:port")
	}
	if !strings.HasPrefix(t.Tunnel, "ws://") && !strings.HasPrefix(t.Tunnel, "wss://") {
		return fmt.Errorf("Remote tunnel must begin with ws:// or wss://")
	}
	t.Tunnel = strings.TrimSuffix(t.Tunnel, "/")

	// validate -server
	if t.InternalServer != nil {
		t.Server = ""
	} else if t.Server != "" {
		if !strings.HasPrefix(t.Server, "http://") && !strings.HasPrefix(t.Server, "https://") {
			return fmt.Errorf("Local server (-server option) must begin with http:// or https://")
		}
		t.Server = strings.TrimSuffix(t.Server, "/")
	}

	// validate token and timeout
	if t.Token == "" {
		return fmt.Errorf("Must specify rendez-vous token using -token option")
	}

	// TODO: http.Client -> CheckRedirect
	// until then, this condition should not be satisfied
	if t.Insecure {
		log15.Info("Accepting unverified SSL certs from local HTTPS servers")
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
		httpClient = http.Client{Transport: tr}
	}

	if t.InternalServer != nil {
		log15.Info("Dispatching to internal server")
	} else if t.Server != "" || t.Regexp != nil {
		log15.Info("Dispatching to external server(s)", "server", t.Server, "regexp", t.Regexp)
	} else {
		return fmt.Errorf("Must specify internal server or server or regexp")
	}

	if t.Proxy != nil {
		username := "(none)"
		if u := t.Proxy.User; u != nil {
			username = u.Username()
		}
		log15.Info("Using HTTPS proxy", "url", t.Proxy.Host, "user", username)
	}

	// for test purposes we have a signal that tells wstuncli to exit instead of reopening
	// a fresh connection.
	t.exitChan = make(chan struct{}, 1)

	//===== Goroutine =====

	// Keep opening websocket connections to tunnel requests
	go func() {
		for {
			d := &websocket.Dialer{
				NetDial:         t.wsProxyDialer,
				ReadBufferSize:  100 * 1024,
				WriteBufferSize: 100 * 1024,
			}
			h := make(http.Header)
			h.Add("Origin", t.Token)
			url := fmt.Sprintf("%s/_tunnel", t.Tunnel)
			timer := time.NewTimer(10 * time.Second)
			log15.Info("WS   Opening", "url", url, "token", t.Token)
			ws, resp, err := d.Dial(url, h)
			if err != nil {
				extra := ""
				if resp != nil {
					extra = resp.Status
					buf := make([]byte, 80)
					resp.Body.Read(buf)
					if len(buf) > 0 {
						extra = extra + " -- " + string(buf)
					}
					resp.Body.Close()
				}
				log15.Error("Error opening connection",
					"err", err.Error(), "info", extra)
			} else {
				t.conn = &WSConnection{ws: ws, tun: t}
				// Safety setting
				ws.SetReadLimit(100 * 1024 * 1024)
				// Request Loop
				srv := t.Server
				if t.InternalServer != nil {
					srv = "<internal>"
				}
				log15.Info("WS   ready", "server", srv)
				t.Connected = true
				t.conn.handleRequests()
				t.Connected = false
			}
			// check whether we need to exit
			select {
			case <-t.exitChan:
				break
			default: // non-blocking receive
			}

			<-timer.C // ensure we don't open connections too rapidly
		}
	}()

	return nil
}

func (t *WSTunnelClient) Stop() {
	t.exitChan <- struct{}{}
}

// Main function to handle WS requests: it reads a request from the socket, then forks
// a goroutine to perform the actual http request and return the result
func (wsc *WSConnection) handleRequests() {
	go wsc.pinger()
	for {
		wsc.ws.SetReadDeadline(time.Time{}) // separate ping-pong routine does timeout
		typ, r, err := wsc.ws.NextReader()
		if err != nil {
			log15.Info("WS   ReadMessage", "err", err.Error())
			break
		}
		if typ != websocket.BinaryMessage {
			log15.Warn("WS   invalid message type", "type", typ)
			break
		}
		// give the sender a minute to produce the request
		wsc.ws.SetReadDeadline(time.Now().Add(time.Minute))
		// read request id
		var id int16
		_, err = fmt.Fscanf(io.LimitReader(r, 4), "%04x", &id)
		if err != nil {
			log15.Warn("WS   cannot read request ID", "err", err.Error())
			break
		}
		// read the whole message, this is bounded (to something large) by the
		// SetReadLimit on the websocket. We have to do this because we want to handle
		// the request in a goroutine (see "go finish..Request" calls below) and the
		// websocket doesn't allow us to have multiple goroutines reading...
		buf, err := ioutil.ReadAll(r)
		if err != nil {
			log15.Warn("WS   cannot read request message", "id", id, "err", err.Error())
			break
		}
		if len(buf) > 1024*1024 {
			log15.Info("WS   long message", "len", len(buf))
		}
		log15.Debug("WS   message", "len", len(buf))
		r = bytes.NewReader(buf)
		// read request itself
		req, err := http.ReadRequest(bufio.NewReader(r))
		if err != nil {
			log15.Warn("WS   cannot read request body", "id", id, "err", err.Error())
			break
		}
		// Hand off to goroutine to finish off while we read the next request
		if wsc.tun.InternalServer != nil {
			go wsc.finishInternalRequest(id, req)
		} else {
			go wsc.finishRequest(id, req)
		}
	}
	// delay a few seconds to allow for writes to drain and then force-close the socket
	go func() {
		time.Sleep(5 * time.Second)
		wsc.ws.Close()
	}()
}

//===== Keep-alive ping-pong =====

// Pinger that keeps connections alive and terminates them if they seem stuck
func (wsc *WSConnection) pinger() {
	defer func() {
		// panics may occur in WriteControl (in unit tests at least) for closed
		// websocket connections
		if x := recover(); x != nil {
			log15.Error("Panic in pinger", "err", x)
		}
	}()
	log15.Info("pinger starting")
	tunTimeout := wsc.tun.Timeout

	// timeout handler sends a close message, waits a few seconds, then kills the socket
	timeout := func() {
		if wsc.ws == nil {
			return
		}
		wsc.ws.WriteControl(websocket.CloseMessage, nil, time.Now().Add(1*time.Second))
		log15.Info("ping timeout, closing WS")
		time.Sleep(5 * time.Second)
		if wsc.ws != nil {
			wsc.ws.Close()
		}
	}
	// timeout timer
	timer := time.AfterFunc(tunTimeout, timeout)
	// pong handler resets last pong time
	ph := func(message string) error {
		timer.Reset(tunTimeout)
		if sf := wsc.tun.StatusFd; sf != nil {
			sf.Seek(0, 0)
			wsc.writeStatus()
			pos, _ := sf.Seek(0, 1)
			sf.Truncate(pos)
		}
		return nil
	}
	wsc.ws.SetPongHandler(ph)
	// ping loop, ends when socket is closed...
	for {
		if wsc.ws == nil {
			break
		}
		err := wsc.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(tunTimeout/3))
		if err != nil {
			break
		}
		time.Sleep(tunTimeout / 3)
	}
	log15.Info("pinger ending (WS errored or closed)")
	wsc.ws.Close()
}

func (wsc *WSConnection) writeStatus() {
	fmt.Fprintf(wsc.tun.StatusFd, "Unix: %d\n", time.Now().Unix())
	fmt.Fprintf(wsc.tun.StatusFd, "Time: %s\n", time.Now().UTC().Format(time.RFC3339))
}

//===== Proxy support =====
// Bits of this taken from golangs net/http/transport.go. Gorilla websocket lib
// allows you to pass in a custom net.Dial function, which it will call instead
// of net.Dial. net.Dial normally just opens up a tcp socket for you. We go one
// extra step and issue an HTTP CONNECT command after the socket is open. After
// HTTP CONNECT is issued and successful, we hand the reins back to gorilla,
// which will then set up SSL and handle the websocket UPGRADE request.
// Note this only handles HTTPS connections through the proxy. HTTP requires
// header rewriting.
func (t *WSTunnelClient) wsProxyDialer(network string, addr string) (conn net.Conn, err error) {
	if t.Proxy == nil {
		return net.Dial(network, addr)
	}

	conn, err = net.Dial("tcp", t.Proxy.Host)
	if err != nil {
		err = fmt.Errorf("WS: error connecting to proxy %s: %s", t.Proxy.Host, err.Error())
		return nil, err
	}

	pa := proxyAuth(t.Proxy)

	connectReq := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}

	if pa != "" {
		connectReq.Header.Set("Proxy-Authorization", pa)
	}
	connectReq.Write(conn)

	// Read and parse CONNECT response.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != 200 {
		//body, _ := ioutil.ReadAll(io.LimitReader(resp.Body, 500))
		//resp.Body.Close()
		//return nil, errors.New("proxy refused connection" + string(body))
		f := strings.SplitN(resp.Status, " ", 2)
		conn.Close()
		return nil, fmt.Errorf(f[1])
	}
	return conn, nil
}

// proxyAuth returns the Proxy-Authorization header to set
// on requests, if applicable.
func proxyAuth(proxy *url.URL) string {
	if u := proxy.User; u != nil {
		username := u.Username()
		password, _ := u.Password()
		return "Basic " + basicAuth(username, password)
	}
	return ""
}

// See 2 (end of page 4) http://www.ietf.org/rfc/rfc2617.txt
func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

//===== HTTP Header Stuff =====

// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te", // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
	"Host",
}

//===== HTTP response writer, used for internal request handlers

type responseWriter struct {
	resp *http.Response
	buf  *bytes.Buffer
}

func newResponseWriter(req *http.Request) *responseWriter {
	buf := bytes.Buffer{}
	resp := http.Response{
		Header:        make(http.Header),
		Body:          ioutil.NopCloser(&buf),
		StatusCode:    -1,
		ContentLength: -1,
		Proto:         req.Proto,
		ProtoMajor:    req.ProtoMajor,
		ProtoMinor:    req.ProtoMinor,
	}
	return &responseWriter{
		resp: &resp,
		buf:  &buf,
	}

}

func (rw *responseWriter) Write(buf []byte) (int, error) {
	if rw.resp.StatusCode == -1 {
		rw.WriteHeader(200)
	}
	return rw.buf.Write(buf)
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.resp.StatusCode = code
	rw.resp.Status = http.StatusText(code)
}

func (rw *responseWriter) Header() http.Header { return rw.resp.Header }

func (rw *responseWriter) finishResponse() error {
	if rw.resp.StatusCode == -1 {
		return fmt.Errorf("HTTP internal handler did not call Write or WriteHeader")
	}
	rw.resp.ContentLength = int64(rw.buf.Len())

	return nil
}

//===== HTTP driver and response sender =====

var wsWriterMutex sync.Mutex // mutex to allow a single goroutine to send a response at a time

// Issue a request to an internal handler. This duplicates some logic found in
// net.http.serve http://golang.org/src/net/http/server.go?#L1124 and
// net.http.readRequest http://golang.org/src/net/http/server.go?#L
func (wsc *WSConnection) finishInternalRequest(id int16, req *http.Request) {
	log := log15.New("id", id, "verb", req.Method, "uri", req.RequestURI)
	log.Debug("HTTP issuing internal request")

	// Remove hop-by-hop headers
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}

	// Add fake protocol version
	req.Proto = "HTTP/1.0"
	req.ProtoMajor = 1
	req.ProtoMinor = 0

	// Dump the request into a buffer in case we want to log it
	dump, _ := httputil.DumpRequest(req, false)
	log.Debug("dump", "req", strings.Replace(string(dump), "\r\n", " || ", -1))

	// Make sure we don't die if a panic occurs in the handler
	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Error("HTTP panic in handler", "err", err, "stack", string(buf))
		}
	}()

	// Concoct Response
	rw := newResponseWriter(req)

	// Issue the request to the HTTP server
	wsc.tun.InternalServer.ServeHTTP(rw, req)

	err := rw.finishResponse()
	if err != nil {
		//dump2, _ := httputil.DumpResponse(resp, true)
		//log15.Info("handleWsRequests: request error", "err", err.Error(),
		//	"req", string(dump), "resp", string(dump2))
		log.Info("HTTP request error", "err", err.Error())
		wsc.writeResponseMessage(id, concoctResponse(req, err.Error(), 502))
		return
	}

	log.Debug("HTTP responded", "status", rw.resp.StatusCode)
	wsc.writeResponseMessage(id, rw.resp)
}

func (wsc *WSConnection) finishRequest(id int16, req *http.Request) {

	log := log15.New("id", id, "verb", req.Method, "uri", req.RequestURI)

	// Honor X-Host header
	host := wsc.tun.Server
	xHost := req.Header.Get("X-Host")
	if xHost != "" {
		re := wsc.tun.Regexp
		if re == nil {
			log.Info("WS   got x-host header but no regexp provided")
			wsc.writeResponseMessage(id, concoctResponse(req,
				"X-Host header disallowed by wstunnel cli (no -regexp option)", 403))
			return
		} else if re.FindString(xHost) == xHost {
			host = xHost
		} else {
			log.Info("WS   x-host disallowed by regexp", "x-host", xHost, "regexp",
				re.String(), "match", re.FindString(xHost))
			wsc.writeResponseMessage(id, concoctResponse(req,
				"X-Host header '"+xHost+"' does not match regexp in wstunnel cli",
				403))
			return
		}
	} else if host == "" {
		log.Info("WS   no x-host header and -server not specified")
		wsc.writeResponseMessage(id, concoctResponse(req,
			"X-Host header required by wstunnel cli (no -server option)", 403))
		return
	}
	req.Header.Del("X-Host")

	// Construct the URL for the outgoing request
	var err error
	req.URL, err = url.Parse(fmt.Sprintf("%s%s", host, req.RequestURI))
	if err != nil {
		log.Warn("WS   cannot parse requestURI", "err", err.Error())
		wsc.writeResponseMessage(id, concoctResponse(req, "Cannot parse request URI", 400))
		return
	}
	req.Host = req.URL.Host // we delete req.Header["Host"] further down
	req.RequestURI = ""
	log.Debug("HTTP issuing request", "url", req.URL.String())

	// Remove hop-by-hop headers
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	// Issue the request to the HTTP server
	dump, err := httputil.DumpRequest(req, false)
	log.Debug("dump", "req", strings.Replace(string(dump), "\r\n", " || ", -1))
	if err != nil {
		log.Warn("error dumping request", "err", err.Error())
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		//dump2, _ := httputil.DumpResponse(resp, true)
		//log15.Info("handleWsRequests: request error", "err", err.Error(),
		//	"req", strings.Replace(string(dump), "\r\n", " || ", -1))
		log.Info("HTTP request error", "err", err.Error())
		wsc.writeResponseMessage(id, concoctResponse(req, err.Error(), 502))
		return
	}
	log.Debug("HTTP responded", "status", resp.Status)
	defer resp.Body.Close()

	wsc.writeResponseMessage(id, resp)
}

// Write the response message to the websocket
func (wsc *WSConnection) writeResponseMessage(id int16, resp *http.Response) {
	// Get writer's lock
	wsWriterMutex.Lock()
	defer wsWriterMutex.Unlock()
	// Write response into the tunnel
	wsc.ws.SetWriteDeadline(time.Now().Add(time.Minute))
	w, err := wsc.ws.NextWriter(websocket.BinaryMessage)
	// got an error, reply with a "hey, retry" to the request handler
	if err != nil {
		log15.Warn("WS   NextWriter", "err", err.Error())
		wsc.ws.Close()
		return
	}

	// write the request Id
	_, err = fmt.Fprintf(w, "%04x", id)
	if err != nil {
		log15.Warn("WS   cannot write request Id", "err", err.Error())
		wsc.ws.Close()
		return
	}

	// write the response itself
	err = resp.Write(w)
	if err != nil {
		log15.Warn("WS   cannot write response", "err", err.Error())
		wsc.ws.Close()
		return
	}

	// done
	err = w.Close()
	if err != nil {
		log15.Warn("WS   write-close failed", "err", err.Error())
		wsc.ws.Close()
		return
	}
}

// Create an http Response from scratch, there must be a better way that this but I
// don't know what it is
func concoctResponse(req *http.Request, message string, code int) *http.Response {
	r := http.Response{
		Status:     "Bad Gateway", //strconv.Itoa(code),
		StatusCode: code,
		Proto:      req.Proto,
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		Header:     make(map[string][]string),
		Request:    req,
	}
	body := bytes.NewReader([]byte(message))
	r.Body = ioutil.NopCloser(body)
	r.ContentLength = int64(body.Len())
	r.Header.Add("content-type", "text/plain")
	r.Header.Add("date", time.Now().Format(time.RFC1123))
	r.Header.Add("server", "wstunnel")
	return &r
}
