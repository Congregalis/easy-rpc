package easyrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Congregalis/easy-rpc/codec"
)

type Call struct {
	Seq           uint64
	ServiceMethod string
	Args          interface{}
	Reply         interface{}
	Error         error
	Done          chan *Call
}

func (call *Call) done() {
	call.Done <- call
}

type Client struct {
	cc      codec.Codec
	opt     *Option
	sending sync.Mutex // 互斥锁，保证请求的有序发送
	header  codec.Header

	mu       sync.Mutex       // 保证客户端状态的改变是线程安全的，状态包括 seq, pending, closing, shutdown
	seq      uint64           // 有序分发请求序号
	pending  map[uint64]*Call // 记录发送出去但未完成的请求
	closing  bool
	shutdown bool
}

var _ io.Closer = &Client{}

var ErrorShutdown = errors.New("connection is shut down")

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		// connection is shut down
		return ErrorShutdown
	}
	c.closing = true
	return c.cc.Close()
}

func (c *Client) IsAvailable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closing && !c.shutdown
}

func (c *Client) registerCall(call *Call) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.shutdown {
		return 0, ErrorShutdown
	}

	call.Seq = c.seq
	c.pending[call.Seq] = call
	c.seq++
	return call.Seq, nil
}

func (c *Client) removeCall(seq uint64) *Call {
	c.mu.Lock()
	defer c.mu.Unlock()

	call := c.pending[seq]
	delete(c.pending, seq)
	return call
}

func (c *Client) terminateCalls(err error) {
	c.sending.Lock()
	defer c.sending.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.shutdown = true
	for _, call := range c.pending {
		call.Error = err
		call.done()
	}
}

func (c *Client) receive() {
	var err error
	for err == nil {
		var h codec.Header
		if err = c.cc.ReadHeader(&h); err != nil {
			break
		}

		call := c.removeCall(h.Seq)
		switch {
		case call == nil:
			// 请求没有发送完整或者请求已被取消
			err = c.cc.ReadBody(nil)
		case h.Error != "":
			call.Error = fmt.Errorf(h.Error)
			err = c.cc.ReadBody(nil)
			call.done()
		default:
			err = c.cc.ReadBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done()
		}
	}

	// 此时有错误发生
	c.terminateCalls(err)
}

func (c *Client) send(call *Call) {
	c.sending.Lock()
	defer c.sending.Unlock()

	seq, err := c.registerCall(call)
	if err != nil {
		call.Error = err
		call.done()
		return
	}

	c.header.ServiceMethod = call.ServiceMethod
	c.header.Seq = seq
	c.header.Error = ""

	if err := c.cc.Write(&c.header, call.Args); err != nil {
		call := c.removeCall(call.Seq)
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

// GO 异步式调用远程方法
func (c *Client) Go(serviceMethod string, args, reply interface{}, done chan *Call) *Call {
	if done == nil {
		done = make(chan *Call, 10)
	} else if cap(done) == 0 {
		log.Panic("[easy-rpc client] done channel is unbuffered")
	}

	call := &Call{
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	c.send(call)
	return call
}

// Call 阻塞式调用远程方法，同时可以通过 context 将控制权交给用户
func (c *Client) Call(ctx context.Context, serviceMethod string, args, reply interface{}) error {
	call := c.Go(serviceMethod, args, reply, make(chan *Call, 1))

	select {
	case call := <-call.Done:
		return call.Error
	case <-ctx.Done():
		c.removeCall(call.Seq)
		return errors.New("[easy-rpc client] call failed: " + ctx.Err().Error())
	}
}

/*
客户端创建与连接相关
*/

func NewClient(conn net.Conn, opt *Option) (*Client, error) {
	f := codec.NewCodecFuncMap[opt.CodecType]
	if f == nil {
		err := fmt.Errorf("invalid codec type %s", opt.CodecType)
		log.Println("[easy-rpc client] codec error:", err)
		return nil, err
	}

	if err := json.NewEncoder(conn).Encode(opt); err != nil {
		log.Println("[easy-rpc client] encoding opt error:", err)
		_ = conn.Close()
		return nil, err
	}

	return newClientCodec(f(conn), opt), nil
}

// 支持 HTTP 协议
func newHTTPClient(conn net.Conn, opt *Option) (*Client, error) {
	_, _ = io.WriteString(conn, fmt.Sprintf("CONNECT %s HTTP/1.0\n\n", defaultRPCPath))

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err == nil && resp.Status == connected {
		return NewClient(conn, opt)
	}

	if err == nil {
		err = errors.New("unexpected HTTP response: " + resp.Status)
	}
	return nil, err
}

func newClientCodec(cc codec.Codec, opt *Option) *Client {
	client := &Client{
		seq:     1,
		cc:      cc,
		opt:     opt,
		pending: make(map[uint64]*Call),
	}
	go client.receive()
	return client
}

// parseOptions 保证 Dial 传入的 Option 合法
func parseOptions(opts ...*Option) (*Option, error) {
	if len(opts) == 0 || opts[0] == nil {
		return DefaultOption, nil
	}

	if len(opts) != 1 {
		return nil, errors.New("number of options is only allowed to be 1")
	}

	opt := opts[0]
	opt.MagicNumber = DefaultOption.MagicNumber
	if opt.CodecType == "" {
		opt.CodecType = DefaultOption.CodecType
	}

	return opt, nil
}

// 连接到 RPC 服务器的统一连接入口
// rpcAddr 格式为 protocol@addr，如
// http@10.0.0.1:8001, tcp@10.0.0.1:8002, unix@/tmp/easyrpc.sock
func XDial(rpcAddr string, opts ...*Option) (*Client, error) {
	parts := strings.Split(rpcAddr, "@")
	if len(parts) != 2 {
		return nil, fmt.Errorf("[easy-rpc client] wrong format '%s', expect protocol@addr", rpcAddr)
	}

	protocol, addr := parts[0], parts[1]
	switch protocol {
	case "http":
		// http 也是基于 tcp 的
		return DialHTTP("tcp", addr, opts...)
	default:
		return Dial(protocol, addr, opts...)
	}
}

func Dial(network, address string, opts ...*Option) (*Client, error) {
	return dialTimeout(NewClient, network, address, opts...)
}

func DialHTTP(network, address string, opts ...*Option) (*Client, error) {
	return dialTimeout(newHTTPClient, network, address, opts...)
}

type clientResult struct {
	client *Client
	err    error
}

type newClientFunc func(conn net.Conn, opt *Option) (client *Client, err error)

func dialTimeout(f newClientFunc, network, address string, opts ...*Option) (client *Client, err error) {
	opt, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialTimeout(network, address, opt.ConnectTimeout)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	ch := make(chan clientResult)
	go func() {
		client, err := f(conn, opt)
		ch <- clientResult{client: client, err: err}
	}()

	if opt.ConnectTimeout == 0 {
		result := <-ch
		return result.client, result.err
	}

	select {
	case result := <-ch:
		return result.client, result.err
	case <-time.After(opt.ConnectTimeout):
		return nil, fmt.Errorf("[easy-rpc client] connect timeout: except within %s", opt.ConnectTimeout)
	}
}
