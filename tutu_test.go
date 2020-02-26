package tutu

import (
	"context"
	"fmt"
	"log"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestPairPipe(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		args []*Config
	}{
		"memory": {
			args: memPairArgs(),
		},
		"tcp": {
			args: []*Config{
				&Config{Number: 2, Type: "tcp", Source: ":15001", Inputs: []int{1}},
				&Config{Number: 3, Type: "tcp", Remote: "127.0.0.1:15001", Delay: 100 * time.Millisecond, Inputs: []int{4}},
			},
		},
		"udp": {
			args: []*Config{
				&Config{Number: 2, Type: "udp", Remote: "127.0.0.1:15002", Inputs: []int{1}},
				&Config{Number: 3, Type: "udp", Source: ":15002", Inputs: []int{4}},
			},
		},
		"udp_listener_first": {
			args: []*Config{
				&Config{Number: 2, Type: "udp", Source: ":15003", Inputs: []int{1}},
				&Config{Number: 3, Type: "udp", Remote: "127.0.0.1:15003", Delay: 100 * time.Millisecond, Inputs: []int{4}},
			},
		},
		"ws": {
			args: []*Config{
				&Config{Number: 2, Type: "ws", Source: ":15004", Inputs: []int{1}},
				&Config{Number: 3, Type: "ws", Remote: "ws://127.0.0.1:15004", Delay: 100 * time.Millisecond, Inputs: []int{4}},
			},
		},
	}

	for name, tt := range tests {
		tt := tt
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()

			left, p1 := net.Pipe()
			p4, right := net.Pipe()
			go func() {
				<-ctx.Done()
				_ = left.Close()
				_ = right.Close()
			}()

			configs := []*Config{
				&Config{Number: 1, Conn: p1, Inputs: []int{2}},
				&Config{Number: 4, Conn: p4, Inputs: []int{3}},
			}
			configs = append(configs, tt.args...)
			go func() {
				_ = Pipe(ctx, &logger{}, configs...)
			}()

			// Write to left, read from right
			errL := make(chan error)
			defer close(errL)
			go func() {
				send := []byte("hello")
				if _, err := left.Write(send); err != nil {
					errL <- err
					return
				}
				buf := make([]byte, 128)
				n, err := right.Read(buf)
				if err != nil {
					errL <- err
					return
				}
				recv := buf[:n]
				if !reflect.DeepEqual(recv, send) {
					errL <- fmt.Errorf("data missmatch (got %s, want %s)", recv, send)
					return
				}
				errL <- nil
			}()

			// Write to right, read from left
			errR := make(chan error)
			defer close(errR)
			go func() {
				send := []byte("world")
				if _, err := right.Write(send); err != nil {
					errR <- err
					return
				}
				buf := make([]byte, 128)
				n, err := left.Read(buf)
				if err != nil {
					errR <- err
					return
				}
				recv := buf[:n]
				if !reflect.DeepEqual(recv, send) {
					errR <- fmt.Errorf("data missmatch (got %s, want %s)", recv, send)
					return
				}
				errR <- nil
			}()

			// Wait for results
			if err := <-errL; err != nil {
				t.Errorf("left: %v", err)
			}
			if err := <-errR; err != nil {
				t.Errorf("right: %v", err)
			}
		})
	}
}

func memPairArgs() []*Config {
	p2, p3 := net.Pipe()
	return []*Config{
		&Config{Number: 2, Conn: p2, Inputs: []int{1}},
		&Config{Number: 3, Conn: p3, Inputs: []int{4}},
	}
}

type logger struct{}

func (l *logger) Info(msg string) {
	log.Println("[INFO]", msg)
}

func (l *logger) Debug(msg string) {
	log.Println("[DEBUG]", msg)
}
