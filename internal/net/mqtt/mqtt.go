package mqtt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Config holds the MQTT client configuration and topics
type Config struct {
	Pub           string
	Sub           string
	ClientOptions *mqtt.ClientOptions
	Logger        Logger
}

// Dial creates a mqtt client connection with a topic to subscribe and a topic
// to publish
func Dial(ctx context.Context, cfg Config) (net.Conn, error) {
	if cfg.Pub == "" {
		return nil, errors.New("config field \"pub\" cannot be empty")
	}
	if cfg.Sub == "" {
		return nil, errors.New("config field \"sub\" cannot be empty")
	}

	// set logger
	logger := cfg.Logger
	if logger == nil {
		logger = noLogger{}
	}

	// create mqtt client
	client := mqtt.NewClient(cfg.ClientOptions)

	// connect client
	if err := wait(ctx, client.Connect()); err != nil {
		return nil, fmt.Errorf("mqtt: couldn't connect: %w", err)
	}

	// create pipe to write to and read from
	rd, wr := net.Pipe()

	// subscribe to from topic
	if err := wait(ctx, client.Subscribe(cfg.Sub, 0, func(client mqtt.Client, msg mqtt.Message) {
		if _, err := wr.Write(msg.Payload()); err != nil {
			logger.Error(fmt.Errorf("mqtt: couldn't write: %w", err))
			_ = wr.Close()
			return
		}
		msg.Ack()
	})); err != nil {
		return nil, err
	}
	return &conn{
		logger:        logger,
		client:        client,
		reader:        rd,
		pub:           cfg.Pub,
		sub:           cfg.Sub,
		writeDeadline: time.Now().Add(10 * time.Second),
	}, nil
}

func wait(ctx context.Context, token mqtt.Token) error {
	for !token.WaitTimeout(500 * time.Millisecond) {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := token.Error(); err != nil {
		return err
	}
	return nil
}

type conn struct {
	logger        Logger
	client        mqtt.Client
	reader        net.Conn
	pub, sub      string
	writeDeadline time.Time
}

// Read implements net.Conn.Read
func (c *conn) Read(b []byte) (n int, err error) {
	return c.reader.Read(b)
}

// Write implements net.Conn.Write
func (c *conn) Write(b []byte) (n int, err error) {
	buf := make([]byte, len(b))
	copy(buf, b)
	ctx, cancel := context.WithDeadline(context.Background(), c.writeDeadline)
	defer cancel()
	if err := wait(ctx, c.client.Publish(c.pub, 0, false, buf)); err != nil {
		return 0, fmt.Errorf("mqtt: couldn't publish: %w", err)
	}
	return len(b), nil
}

// Close implements net.Conn.Close
func (c *conn) Close() error {
	defer c.client.Disconnect(1000)
	if err := c.reader.Close(); err != nil {
		c.logger.Warn(fmt.Errorf("mqtt: reader close failed: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := wait(ctx, c.client.Unsubscribe(c.sub)); err != nil {
		return fmt.Errorf("mqtt: couldn't unsubscribe: %w", err)
	}
	return nil
}

type addr string

func (addr) Network() string  { return "mqtt" }
func (a addr) String() string { return string(a) }

// LocalAddr implements net.Conn.LocalAddr
func (c *conn) LocalAddr() net.Addr {
	return addr(c.sub)
}

// RemoteAddr implements net.Conn.RemoteAddr
func (c *conn) RemoteAddr() net.Addr {
	return addr(c.pub)
}

// SetDeadline implements net.Conn.SetDeadline
func (c *conn) SetDeadline(t time.Time) error {
	if err := c.SetWriteDeadline(t); err != nil {
		c.logger.Warn(fmt.Errorf("mqtt: couldn't set write deadline: %w", err))
	}
	return c.SetReadDeadline(t)
}

// SetReadDeadline implements net.Conn.SetReadDeadline
func (c *conn) SetReadDeadline(t time.Time) error {
	return c.reader.SetReadDeadline(t)
}

// SetWriteDeadline implements net.Conn.SetWriteDeadline
func (c *conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return nil
}

// Logger interface
type Logger interface {
	Trace(args ...interface{})
	Debug(args ...interface{})
	Info(args ...interface{})
	Warn(args ...interface{})
	Error(args ...interface{})
}

type noLogger struct{}

func (noLogger) Trace(args ...interface{}) {}
func (noLogger) Debug(args ...interface{}) {}
func (noLogger) Info(args ...interface{})  {}
func (noLogger) Warn(args ...interface{})  {}
func (noLogger) Error(args ...interface{}) {}
