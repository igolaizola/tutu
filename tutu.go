package tutu

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/igolaizola/rtconn"
	"github.com/igolaizola/tutu/internal/ioasync"
	"github.com/igolaizola/tutu/internal/net/mqtt"
	dtls "github.com/pion/dtls/v2"
	"github.com/pion/dtls/v2/pkg/crypto/selfsign"
	"github.com/pion/logging"
	"nhooyr.io/websocket"
)

// Logger is an interface for logging
type Logger interface {
	Info(string)
	Debug(string)
}

// Config holds the configuration of a stream
type Config struct {
	Number    int
	Type      string
	Inputs    []int
	AutoClose bool
	Delay     time.Duration
	Wait      time.Duration
	Exec      string
	Args      []string
	Hex       bool
	File      string
	EOL       bool
	Remote    string
	Source    string
	Insecure  bool
	Pub       string
	Sub       string
	Protocol  string
	DTLS      string
	DTLSState string
	Header    http.Header
	Conn      net.Conn
}

// Pipe creates pipes between configured streams
func Pipe(ctx context.Context, logger Logger, configs ...*Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// create streams
	var streams []*stream
	wg := sync.WaitGroup{}
	lock := sync.Mutex{}
	for _, cfg := range configs {
		cfg := cfg
		n := cfg.Number
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cfg.Delay > 0 {
				delay, cancel := context.WithTimeout(ctx, cfg.Delay)
				defer cancel()
				<-delay.Done()
				if ctx.Err() != nil {
					return
				}
			}
			strm, err := newStream(ctx, logger, cfg)
			if err != nil {
				logger.Info(fmt.Sprintf("couldn't create stream %d: %v", n, err))
				return
			}
			lock.Lock()
			defer lock.Unlock()
			streams = append(streams, strm)
		}()
	}

	// wait for streams creation
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	logger.Info("streams created")

	// wait groups to wait until there is no more input for a stream
	inputs := make(map[*stream]*sync.WaitGroup)
	for _, s := range streams {
		if s.autoClose {
			inputs[s] = &sync.WaitGroup{}
		}
	}

	// pipe between readers and writers
	for _, reader := range streams {
		// number 0 is skipped
		if reader.number == 0 {
			continue
		}

		var writers []io.Writer
		for _, writer := range streams {
			// do not pipe readers and writer of same group
			if reader.number == writer.number {
				continue
			}
			write := len(writer.inputs) == 0
			for _, input := range writer.inputs {
				if input == reader.number {
					write = true
					break
				}
			}
			if write {
				if in, ok := inputs[writer]; ok {
					in.Add(1)
				}
				writers = append(writers, writer)
			}
		}
		if len(writers) == 0 {
			continue
		}
		r := reader
		bcast := ioasync.NewBroadcastWriter(ctx, writers...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				for _, w := range writers {
					if in, ok := inputs[w.(*stream)]; ok {
						in.Done()
					}
				}
			}()
			// TODO(igolaizola): read buf len from param
			buf := make([]byte, 1024)
			defer func() {
				r.Close()
			}()
			for {
				n, err := r.Read(buf)
				if err != nil {
					logger.Debug(fmt.Sprintf("read from %d failed: %v", r.number, err))
					return
				}
				data := make([]byte, n)
				copy(data, buf[:n])
				go func() {
					if _, err := bcast.Write(data); err != nil {
						logger.Debug(fmt.Sprintf("write failed: %v", err))
					}
				}()
			}
		}()
	}

	// Close streams with no more inputs and auto-close enabled
	for s, wg := range inputs {
		s := s
		wg := wg
		go func() {
			wg.Wait()
			logger.Debug(fmt.Sprintf("closing stream %d", s.number))
			_ = s.Close()
		}()
	}

	// Signal when all readers finished
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	// wait for context cancelation
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

type readWriteCloser interface {
	io.Reader
	io.Writer
	io.Closer
}

type stream struct {
	readWriteCloser
	number    int
	inputs    []int
	autoClose bool
}

// newStream creates a new stream
func newStream(ctx context.Context, logger Logger, cfg *Config) (*stream, error) {
	var rwc readWriteCloser
	number := cfg.Number
	inputs := cfg.Inputs

	// Dial timeout
	timeout := cfg.Wait
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch cfg.Type {
	case "stdio":
		rwc = &contextRWC{
			reader: os.Stdin,
			writer: os.Stdout,
			ctx:    ctx,
		}
	case "file":
		f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_RDWR, os.ModePerm)
		if err != nil {
			return nil, fmt.Errorf("couldn't open file %s: %w", cfg.File, err)
		}
		rwc = &fileRWC{
			File: f,
			ctx:  ctx,
		}
	case "cmd":
		var inner net.Conn
		rwc, inner = net.Pipe()
		cmd := exec.CommandContext(ctx, cfg.Exec, cfg.Args...)
		cmd.Stdin = inner
		cmd.Stdout = inner
		cmd.Stderr = os.Stderr
		go func() {
			if err := cmd.Run(); err != nil {
				logger.Info(fmt.Sprintf("command %s failed: %v", cfg.Exec, err))
			}
			_ = inner.Close()
		}()
	case "http":
		dialer := rtconn.Dialer{Timeout: timeout}
		var err error
		rwc, err = dialer.Dial(ctx, cfg.Remote, cfg.Header)
		if err != nil {
			return nil, fmt.Errorf("couldn't dial %s: %w", cfg.Remote, err)
		}
	case "ws":
		switch {
		case cfg.Source != "":
			errC := make(chan error)
			srv := &http.Server{
				Addr: cfg.Source,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wsconn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
						InsecureSkipVerify: true,
					})
					rwc = websocket.NetConn(ctx, wsconn, websocket.MessageBinary)
					errC <- err
				}),
			}
			go func() {
				_ = srv.ListenAndServe()
			}()

			select {
			case <-dialCtx.Done():
				defer func() { _ = srv.Close() }()
				return nil, fmt.Errorf("couldn't accept websocket connection: timeout")
			case err := <-errC:
				go func() {
					<-ctx.Done()
					_ = srv.Close()
				}()
				if err != nil {
					return nil, fmt.Errorf("couldn't accept websocket connection: %w", err)
				}
			}
		case cfg.Remote != "":
			var protos []string
			if cfg.Protocol != "" {
				protos = append(protos, cfg.Protocol)
			}
			var client *http.Client
			if cfg.Insecure {
				client = http.DefaultClient
				client.Transport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
			}
			wsconn, _, err := websocket.Dial(dialCtx, cfg.Remote, &websocket.DialOptions{
				HTTPHeader:   cfg.Header,
				Subprotocols: protos,
			})
			if err != nil {
				return nil, fmt.Errorf("couldn't dial ws %s: %w", cfg.Remote, err)
			}
			rwc = websocket.NetConn(ctx, wsconn, websocket.MessageBinary)
		default:
			return nil, fmt.Errorf("websocket address not provided")
		}
	case "mqtt":
		// create client options with a random client id
		opts := pahomqtt.NewClientOptions()
		opts.AddBroker(cfg.Remote)
		opts.SetHTTPHeaders(cfg.Header)
		opts.SetTLSConfig(&tls.Config{
			InsecureSkipVerify: cfg.Insecure,
		})
		rnd := make([]byte, 6)
		if _, err := rand.Read(rnd); err != nil {
			return nil, fmt.Errorf("couldn't generate random client id: %w", err)
		}
		opts.SetClientID(fmt.Sprintf("tutu-%x", rnd))
		var err error
		rwc, err = mqtt.Dial(dialCtx, mqtt.Config{
			Pub:           cfg.Pub,
			Sub:           cfg.Sub,
			ClientOptions: opts,
		})
		if err != nil {
			return nil, fmt.Errorf("couldn't dial mqtt %s: %w", cfg.Remote, err)
		}
	case "udp":
		var laddr, raddr *net.UDPAddr
		var err error
		if cfg.Remote != "" {
			raddr, err = net.ResolveUDPAddr("udp", cfg.Remote)
			if err != nil {
				return nil, fmt.Errorf("couldn't resolve udp addr: %s", cfg.Remote)
			}
		}
		if cfg.Source != "" {
			laddr, err = net.ResolveUDPAddr("udp", cfg.Source)
			if err != nil {
				return nil, fmt.Errorf("couldn't resolve udp addr: %s", cfg.Source)
			}
		}
		switch {
		case raddr != nil:
			dialer := net.Dialer{
				LocalAddr: laddr,
			}
			rwc, err = dialer.DialContext(dialCtx, "udp", raddr.String())
			if err != nil {
				return nil, fmt.Errorf("couldn't dial udp %s: %w", raddr, err)
			}
		case laddr != nil:
			uConn, err := net.ListenUDP("udp", laddr)
			if err != nil {
				return nil, fmt.Errorf("couldn't listen udp %s: %w", laddr, err)
			}
			rwc = newUDPConn(ctx, uConn)
		default:
			return nil, fmt.Errorf("udp address not provided")
		}
	case "tcp":
		var laddr, raddr *net.TCPAddr
		var err error
		if cfg.Remote != "" {
			raddr, err = net.ResolveTCPAddr("tcp", cfg.Remote)
			if err != nil {
				return nil, fmt.Errorf("couldn't resolve tcp addr: %s", cfg.Remote)
			}
		}
		if cfg.Source != "" {
			laddr, err = net.ResolveTCPAddr("tcp", cfg.Source)
			if err != nil {
				return nil, fmt.Errorf("couldn't resolve tcp addr: %s", cfg.Source)
			}
		}
		switch {
		case raddr != nil:
			dialer := net.Dialer{
				LocalAddr: laddr,
			}
			rwc, err = dialer.DialContext(dialCtx, "tcp", raddr.String())
			if err != nil {
				return nil, fmt.Errorf("couldn't dial tcp %s: %w", raddr, err)
			}
		case laddr != nil:
			listener, err := net.ListenTCP("tcp", laddr)
			if err != nil {
				return nil, fmt.Errorf("couldn't listen tcp %s: %w", laddr, err)
			}
			go func() {
				<-dialCtx.Done()
				listener.Close()
			}()
			rwc, err = listener.Accept()
			if err != nil {
				return nil, fmt.Errorf("couldn't accept tcp connection: %w", err)
			}
		default:
			return nil, fmt.Errorf("tcp address not provided")
		}
	default:
		if cfg.Conn != nil {
			rwc = cfg.Conn
		} else {
			return nil, fmt.Errorf("type %s not supported", cfg.Type)
		}
	}

	// apply dtls
	if cfg.DTLS != "" {
		var err error
		conn, ok := rwc.(net.Conn)
		if !ok {
			conn = &rwcConn{rwc: rwc, network: cfg.Type}
		}
		rwc, err = dtlsConn(logger, conn, cfg)
		if err != nil {
			return nil, err
		}
	}

	// apply new line
	if cfg.EOL {
		rwc = &newLineRWC{readWriteCloser: rwc}
	}

	// apply hex encoding
	if cfg.Hex {
		rwc = &hexRWC{rwc: rwc}
	}

	return &stream{
		readWriteCloser: rwc,
		number:          number,
		inputs:          inputs,
		autoClose:       cfg.AutoClose,
	}, nil
}

// dtlsConn is a wrapper that adds dtls security layer
func dtlsConn(logger Logger, conn net.Conn, cfg *Config) (net.Conn, error) {
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return nil, fmt.Errorf("couldn't generate certificate: %w", err)
	}
	dtlsCfg := &dtls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		LoggerFactory:      &dtlsLogger{logger: logger},
	}
	var dtlsConn *dtls.Conn
	switch {
	case cfg.DTLSState != "":
		data, err := hex.DecodeString(cfg.DTLSState)
		if err != nil {
			return nil, fmt.Errorf("couldn't decode state: %w", err)
		}
		state := &dtls.State{}
		if err := state.UnmarshalBinary(data); err != nil {
			return nil, fmt.Errorf("couldn't unmarshal state: %w", err)
		}
		dtlsConn, err = dtls.Resume(state, conn, dtlsCfg)
		if err != nil {
			return nil, fmt.Errorf("couldn't resume conn: %w", err)
		}
		logger.Debug("dtls connection resumed")
	case cfg.DTLS == "client":
		dtlsConn, err = dtls.Client(conn, dtlsCfg)
		if err != nil {
			return nil, fmt.Errorf("couldn't create client conn: %w", err)
		}
		logger.Debug("dtls client connected")
	case cfg.DTLS == "server":
		dtlsConn, err = dtls.Server(conn, dtlsCfg)
		if err != nil {
			return nil, fmt.Errorf("couldn't create server conn: %w", err)
		}
		logger.Debug("dtls server connected")
	default:
		return nil, fmt.Errorf("dtls value %s not supported", cfg.DTLS)
	}
	return dtlsConn, nil
}

// newUDPConn is a wrapper func for udp listners with unknown remote addr
func newUDPConn(ctx context.Context, c *net.UDPConn) net.Conn {
	uConn := &udpConn{
		UDPConn: c,
		first:   make(chan struct{}),
	}
	return &overrideConn{
		Conn:   uConn,
		writer: ioasync.NewAsyncWriter(ctx, uConn),
	}
}

type udpConn struct {
	*net.UDPConn
	addr  *net.UDPAddr
	first chan struct{}
}

func (c *udpConn) Read(b []byte) (int, error) {
	n, addr, err := c.UDPConn.ReadFromUDP(b)
	if err != nil {
		return 0, err
	}
	if c.addr == nil {
		defer close(c.first)
	}
	c.addr = addr
	return n, nil
}

func (c *udpConn) Write(b []byte) (int, error) {
	<-c.first
	return c.UDPConn.WriteToUDP(b, c.addr)
}

// overrideConn is a wrapper that overrides read or write methods
type overrideConn struct {
	net.Conn
	reader io.Reader
	writer io.Writer
}

func (c *overrideConn) Read(b []byte) (int, error) {
	if c.reader != nil {
		return c.reader.Read(b)
	}
	return c.Conn.Read(b)
}

func (c *overrideConn) Write(b []byte) (int, error) {
	if c.writer != nil {
		return c.writer.Write(b)
	}
	return c.Conn.Write(b)
}

// hexRWC is a wrapper that encodes decodes hexadecimal strings
type hexRWC struct {
	rwc readWriteCloser
}

func (c *hexRWC) Read(b []byte) (int, error) {
	n, err := c.rwc.Read(b)
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(b[:n]))
	data, err := hex.DecodeString(text)
	if err != nil {
		return 0, fmt.Errorf("couldn't decode hex data: %w", err)
	}
	copy(b, data)
	return len(data), nil
}

func (c *hexRWC) Write(b []byte) (int, error) {
	data := []byte(hex.EncodeToString(b))
	if _, err := c.rwc.Write(data); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *hexRWC) Close() error {
	return c.rwc.Close()
}

// newLineWriter is a wrapper over a writer that adds new lines
type newLineRWC struct {
	readWriteCloser
}

func (c *newLineRWC) Write(b []byte) (int, error) {
	n, err := c.readWriteCloser.Write(b)
	if err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintln(c.readWriteCloser, ""); err != nil {
		return 0, err
	}
	return n, nil
}

// fileRWC is readWriteColser implementation using a file
type fileRWC struct {
	*os.File
	ctx context.Context
}

func (c *fileRWC) Read(b []byte) (n int, err error) {
	for {
		n, err = c.File.Read(b)
		if !errors.Is(err, io.EOF) {
			return n, err
		}
		select {
		case <-c.ctx.Done():
			return 0, io.EOF
		case <-time.After(100 * time.Millisecond):
		}

	}
}

// contextRWC is a custom readWriteCloser implementation
type contextRWC struct {
	reader io.Reader
	writer io.Writer
	closer io.Closer
	ctx    context.Context
}

func (c *contextRWC) Read(b []byte) (n int, err error) {
	done := make(chan struct{})
	go func() {
		n, err = c.reader.Read(b)
		close(done)
	}()
	select {
	case <-c.ctx.Done():
		return 0, io.EOF
	case <-done:
	}
	return n, err
}

func (c *contextRWC) Write(b []byte) (n int, err error) {
	return c.writer.Write(b)
}

func (c *contextRWC) Close() error {
	if c.closer == nil {
		return nil
	}
	return c.closer.Close()
}

// rwcConn is net.Conn implementation using a given reader and writer
type rwcConn struct {
	rwc     readWriteCloser
	network string
}

func (c *rwcConn) Read(b []byte) (n int, err error)   { return c.rwc.Read(b) }
func (c *rwcConn) Write(b []byte) (n int, err error)  { return c.rwc.Write(b) }
func (c *rwcConn) Close() error                       { return c.rwc.Close() }
func (c *rwcConn) LocalAddr() net.Addr                { return addr(c.network) }
func (c *rwcConn) RemoteAddr() net.Addr               { return addr(c.network) }
func (c *rwcConn) SetDeadline(t time.Time) error      { return nil }
func (c *rwcConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *rwcConn) SetWriteDeadline(t time.Time) error { return nil }

type addr string

func (a addr) Network() string { return string(a) }
func (addr) String() string    { return "localhost" }

// dtlsLogger is wrapper of logrus that satisfies logging.LeveledLogger
type dtlsLogger struct {
	logger Logger
}

func (l *dtlsLogger) NewLogger(string) logging.LeveledLogger { return l }

func (l *dtlsLogger) Trace(msg string)                          { l.Debug(msg) }
func (l *dtlsLogger) Tracef(format string, args ...interface{}) { l.Debugf(format, args...) }
func (l *dtlsLogger) Debug(msg string)                          { l.logger.Debug(msg) }
func (l *dtlsLogger) Debugf(format string, args ...interface{}) {
	l.logger.Debug(fmt.Sprintf(format, args...))
}
func (l *dtlsLogger) Info(msg string) { l.logger.Info(msg) }
func (l *dtlsLogger) Infof(format string, args ...interface{}) {
	l.logger.Info(fmt.Sprintf(format, args...))
}
func (l *dtlsLogger) Warn(msg string)                           { l.Info(msg) }
func (l *dtlsLogger) Warnf(format string, args ...interface{})  { l.Infof(format, args...) }
func (l *dtlsLogger) Error(msg string)                          { l.Info(msg) }
func (l *dtlsLogger) Errorf(format string, args ...interface{}) { l.Infof(format, args...) }
