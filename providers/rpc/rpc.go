// Package rpc implements bounded, bidirectional newline-delimited RPC
// for native agent protocols. It does not execute commands or grant permissions.
package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"unicode/utf8"
)

type Message struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("provider RPC error %d: %s", e.Code, e.Message) }

type Handler func(context.Context, *Peer, Message) error
type Options struct {
	OmitVersion   bool
	MaxFrameBytes int
	MaxPending    int
}

var ErrClosed = errors.New("provider RPC connection closed")

type frame struct {
	Version string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}
type reply struct {
	result json.RawMessage
	err    error
}
type writeRequest struct {
	ctx  context.Context
	data []byte
	done chan error
}
type Peer struct {
	ctx       context.Context
	cancel    context.CancelFunc
	transport io.ReadWriteCloser
	options   Options
	handler   Handler
	mu        sync.Mutex
	pending   map[string]chan reply
	next      uint64
	err       error
	writes    chan writeRequest
	done      chan struct{}
	once      sync.Once
	workers   sync.WaitGroup
}

// New starts owned reader/writer loops. The handler runs in wire order before
// responses are delivered, so a completed prompt cannot overtake its updates.
// Handlers may Respond/Notify, but must not synchronously Call or Close this peer
// and must honor context cancellation when forwarding events to their consumer.
func New(parent context.Context, transport io.ReadWriteCloser, options Options, handler Handler) *Peer {
	if options.MaxFrameBytes <= 0 {
		options.MaxFrameBytes = 4 << 20
	}
	if options.MaxPending <= 0 {
		options.MaxPending = 256
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Peer{ctx: ctx, cancel: cancel, transport: transport, options: options, handler: handler, pending: map[string]chan reply{}, writes: make(chan writeRequest, 32), done: make(chan struct{})}
	p.workers.Add(3)
	go p.readLoop()
	go p.writeLoop()
	go func() {
		defer p.workers.Done()
		select {
		case <-ctx.Done():
			p.abort(ctx.Err())
		case <-p.done:
		}
	}()
	return p
}
func (p *Peer) Done() <-chan struct{} { return p.done }
func (p *Peer) Err() error            { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *Peer) Close() error          { p.abort(ErrClosed); p.workers.Wait(); return nil }
func (p *Peer) abort(err error) {
	p.once.Do(func() {
		if err == nil {
			err = ErrClosed
		}
		p.mu.Lock()
		p.err = err
		for key, ch := range p.pending {
			ch <- reply{err: err}
			delete(p.pending, key)
		}
		p.mu.Unlock()
		p.cancel()
		close(p.done)
		p.transport.Close()
	})
}
func (p *Peer) version() string {
	if p.options.OmitVersion {
		return ""
	}
	return "2.0"
}
func marshalParams(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	return data, err
}

func (p *Peer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "" {
		return nil, fmt.Errorf("provider RPC method is required")
	}
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return nil, err
	}
	if len(p.pending) >= p.options.MaxPending {
		p.mu.Unlock()
		return nil, fmt.Errorf("provider RPC pending request limit reached")
	}
	p.next++
	key := strconv.FormatUint(p.next, 10)
	result := make(chan reply, 1)
	p.pending[key] = result
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, key); p.mu.Unlock() }()
	err = p.send(ctx, frame{Version: p.version(), ID: json.RawMessage(key), Method: method, Params: raw})
	if err != nil {
		select {
		case response := <-result:
			return response.result, response.err
		default:
			return nil, err
		}
	}
	select {
	case response := <-result:
		return response.result, response.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (p *Peer) Notify(ctx context.Context, method string, params any) error {
	if method == "" {
		return fmt.Errorf("provider RPC method is required")
	}
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return p.send(ctx, frame{Version: p.version(), Method: method, Params: raw})
}
func (p *Peer) Respond(ctx context.Context, id json.RawMessage, result any, rpcError *Error) error {
	if !validID(id) {
		return fmt.Errorf("invalid provider RPC response ID")
	}
	f := frame{Version: p.version(), ID: id, Error: rpcError}
	if rpcError == nil {
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		f.Result = raw
	}
	return p.send(ctx, f)
}
func (p *Peer) send(ctx context.Context, f frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(data) > p.options.MaxFrameBytes {
		return fmt.Errorf("outbound provider RPC frame exceeds limit")
	}
	request := writeRequest{ctx: ctx, data: append(data, '\n'), done: make(chan error, 1)}
	select {
	case p.writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return p.Err()
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		select {
		case err := <-request.done:
			return err
		default:
			return p.Err()
		}
	}
}
func (p *Peer) writeLoop() {
	defer p.workers.Done()
	for {
		select {
		case <-p.done:
			return
		case request := <-p.writes:
			if err := request.ctx.Err(); err != nil {
				request.done <- err
				continue
			}
			data := request.data
			for len(data) > 0 {
				n, err := p.transport.Write(data)
				if err != nil {
					request.done <- err
					p.abort(err)
					return
				}
				if n == 0 {
					request.done <- io.ErrShortWrite
					p.abort(io.ErrShortWrite)
					return
				}
				data = data[n:]
			}
			request.done <- nil
		}
	}
}
func validID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	if bytes.Equal(raw, []byte("null")) {
		return true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return true
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&number) != nil {
		return false
	}
	_, err := strconv.ParseInt(number.String(), 10, 64)
	return err == nil
}
func (p *Peer) readLoop() {
	defer p.workers.Done()
	scanner := bufio.NewScanner(p.transport)
	initial := 4096
	if initial > p.options.MaxFrameBytes {
		initial = p.options.MaxFrameBytes
	}
	scanner.Buffer(make([]byte, initial), p.options.MaxFrameBytes+1)
	for scanner.Scan() {
		data := scanner.Bytes()
		if len(data) > p.options.MaxFrameBytes || !utf8.Valid(data) {
			p.abort(fmt.Errorf("provider RPC frame exceeds limit or contains invalid UTF-8"))
			return
		}
		var f frame
		if len(data) == 0 || json.Unmarshal(data, &f) != nil || (f.Version != "2.0" && !(p.options.OmitVersion && f.Version == "")) {
			p.abort(fmt.Errorf("provider sent malformed RPC frame"))
			return
		}
		if len(f.ID) > 0 && !validID(f.ID) {
			p.abort(fmt.Errorf("provider sent invalid RPC ID"))
			return
		}
		if f.Method != "" {
			if f.Result != nil || f.Error != nil {
				p.abort(fmt.Errorf("provider sent mixed RPC request/response"))
				return
			}
			m := Message{ID: f.ID, Method: f.Method, Params: f.Params}
			if p.handler != nil {
				if err := p.handler(p.ctx, p, m); err != nil {
					p.abort(err)
					return
				}
			} else if f.ID != nil {
				if err := p.Respond(p.ctx, f.ID, nil, &Error{Code: -32601, Message: "method not supported"}); err != nil {
					p.abort(err)
					return
				}
			}
			continue
		}
		if f.ID == nil || (f.Result == nil) == (f.Error == nil) {
			p.abort(fmt.Errorf("provider sent invalid RPC response"))
			return
		}
		p.mu.Lock()
		ch := p.pending[string(f.ID)]
		if ch != nil {
			delete(p.pending, string(f.ID))
			r := reply{result: f.Result}
			if f.Error != nil {
				r.err = f.Error
			}
			ch <- r
		}
		p.mu.Unlock()
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	p.abort(err)
}
