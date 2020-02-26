package ioasync

import (
	"context"
	"io"
	"sync"
)

type asyncWriter struct {
	ctx    context.Context
	writer io.Writer
	lock   sync.Mutex
	data   chan []byte
	buffer [][]byte
	err    error
}

// NewAsyncWriter creates an asynchronous writer
func NewAsyncWriter(ctx context.Context, w io.Writer) io.Writer {
	ctx, cancel := context.WithCancel(ctx)
	a := asyncWriter{
		ctx:    ctx,
		writer: w,
		lock:   sync.Mutex{},
		data:   make(chan []byte),
	}
	go func() {
		defer func() {
			cancel()
			<-ctx.Done()
			close(a.data)
		}()
		for {
			select {
			case <-a.ctx.Done():
				return
			default:
			}
			var data []byte
			a.lock.Lock()
			switch len(a.buffer) {
			case 0:
				a.lock.Unlock()
				select {
				case <-a.ctx.Done():
					return
				case data = <-a.data:
				}
			default:
				data = a.buffer[0]
				a.buffer = a.buffer[1:]
				a.lock.Unlock()
			}
			buf := make([]byte, len(data))
			copy(buf, data)
			if _, err := a.writer.Write(buf); err != nil {
				a.err = err
				return
			}
		}
	}()
	return &a
}

func (a *asyncWriter) Write(b []byte) (int, error) {
	select {
	case <-a.ctx.Done():
		if a.err != nil {
			return 0, a.err
		}
		return 0, a.ctx.Err()
	case a.data <- b:
	default:
		a.lock.Lock()
		defer a.lock.Unlock()
		a.buffer = append(a.buffer, b)
	}
	return len(b), nil
}

// NewBroadcastWriter creates a writer that writes to multiple asynchronous
// writers
func NewBroadcastWriter(ctx context.Context, writers ...io.Writer) io.Writer {
	var aWriters []io.Writer
	for _, w := range writers {
		aWriters = append(aWriters, NewAsyncWriter(ctx, w))
	}
	return io.MultiWriter(aWriters...)
}
