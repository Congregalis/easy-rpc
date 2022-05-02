package easyrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Congregalis/easy-rpc/codec"
)

/*
服务端相关
*/

const MagicNumber = 0x3f3f3f

type Option struct {
	MagicNumber    int
	CodecType      codec.Type
	ConnectTimeout time.Duration
	HandleTimeout  time.Duration
}

var DefaultOption = &Option{
	MagicNumber:    MagicNumber,
	CodecType:      codec.GobType,
	ConnectTimeout: 10 * time.Second,
}

type Server struct {
	serviceMap sync.Map
}

func NewServer() *Server {
	return &Server{}
}

var DefaultServer = NewServer()

func (s *Server) Register(rcvr interface{}) error {
	service := newService(rcvr)
	if _, dup := s.serviceMap.LoadOrStore(service.name, service); dup {
		return errors.New("[easy-rpc server] service already exist: " + service.name)
	}

	return nil
}

func Register(rcvr interface{}) error { return DefaultServer.Register(rcvr) }

func (s *Server) Accept(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			log.Println("[easy-rpc server] accept error:", err)
			return
		}

		go s.ServeConn(conn)
	}
}

func (s *Server) findService(serviceMethod string) (svc *service, mtype *methodType, err error) {
	dot := strings.LastIndex(serviceMethod, ".")
	if dot < 0 {
		err = errors.New("[easy-rpc server] service method request ill-formed: " + serviceMethod)
		return
	}

	serviceName, methodName := serviceMethod[:dot], serviceMethod[dot+1:]
	svci, ok := s.serviceMap.Load(serviceName)
	if !ok {
		err = errors.New("[easy-rpc server] can't find service: " + serviceName)
		return
	}
	svc = svci.(*service)
	mtype = svc.method[methodName]
	if mtype == nil {
		err = errors.New("[easy-rpc server] can't find method: " + methodName)
	}

	return
}

func Accept(lis net.Listener) { DefaultServer.Accept(lis) }

func (s *Server) ServeConn(conn io.ReadWriteCloser) {
	defer func() { _ = conn.Close() }()

	var opt Option
	if err := json.NewDecoder(conn).Decode(&opt); err != nil {
		log.Println("[easy-rpc server] decode option error:", err)
		return
	}

	if opt.MagicNumber != MagicNumber {
		log.Println("[easy-rpc server] invalid magic number:", opt.MagicNumber)
		return
	}

	f := codec.NewCodecFuncMap[opt.CodecType]
	if f == nil {
		log.Println("[easy-rpc server] invalid codec type:", opt.CodecType)
		return
	}

	s.ServeCodec(f(conn), &opt)
}

// placeholder for response of invalid request
var invalidRequest = struct{}{}

func (s *Server) ServeCodec(cc codec.Codec, opt *Option) {
	defer func() { _ = cc.Close() }()

	sending := new(sync.Mutex)
	wg := new(sync.WaitGroup)

	for {
		req, err := s.readRequest(cc)
		if err != nil {
			if req == nil {
				break
			}

			req.h.Error = err.Error()
			s.sendResponse(cc, req.h, invalidRequest, sending)
			continue
		}

		wg.Add(1)
		go s.handleRequest(cc, req, sending, wg, opt.HandleTimeout)
	}

	wg.Wait()
	_ = cc.Close()
}

/*
请求体相关
*/

type request struct {
	h            *codec.Header
	argv, replyv reflect.Value
	mtype        *methodType
	svc          *service
}

func (s *Server) readRequestHeader(cc codec.Codec) (*codec.Header, error) {
	var h codec.Header
	if err := cc.ReadHeader(&h); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			log.Println("[easy-rpc server] read header error:", err)
		}
		return nil, err
	}

	return &h, nil
}

func (s *Server) readRequest(cc codec.Codec) (*request, error) {
	// [~TODO] 此处如果不存在服务，会先报如下错误：
	// [easy-rpc server] read header error: gob: type mismatch: no fields matched compiling decoder for Header
	// 而不是不存在服务
	// [初步 Solved] 因为客户端处收到 err 后没有执行done()，也就是没有往管道传送值，导致 Client.Call() 一直阻塞

	// [TODO] 程序运行正确了但仍会出现上述错误，猜测请求发送了不止一次，需要进一步排查
	// 猜测 cc.ReadHeader(&h) 即使没有数据传过来也会读，所以会又再次读一遍 (目前看来似乎是这样，只能想办法阻塞等下一个 call)

	h, err := s.readRequestHeader(cc)
	if err != nil {
		return nil, err
	}

	req := &request{h: h}

	req.svc, req.mtype, err = s.findService(h.ServiceMethod)
	if err != nil {
		return req, err
	}

	req.argv = req.mtype.newArgv()
	req.replyv = req.mtype.newReplyv()

	// argv 可能是指针也可能是值，由于 cc.ReadBody 需要他作为指针，所以以下代码确保传入的是指针
	argvi := req.argv.Interface()
	if req.argv.Type().Kind() != reflect.Ptr {
		argvi = req.argv.Addr().Interface()
	}

	if err := cc.ReadBody(argvi); err != nil {
		log.Println("[easy-rpc server] read argv error:", err)
		return req, err
	}

	return req, nil
}

func (s *Server) sendResponse(cc codec.Codec, h *codec.Header, body interface{}, sending *sync.Mutex) {
	sending.Lock()
	defer sending.Unlock()

	if err := cc.Write(h, body); err != nil {
		log.Println("[easy-rpc server] write response error:", err)
	}
}

func (s *Server) handleRequest(cc codec.Codec, req *request, sending *sync.Mutex, wg *sync.WaitGroup, timeout time.Duration) {
	defer wg.Done()
	called := make(chan struct{})                           // called 阶段，表示调用服务阶段
	sent := make(chan struct{})                             // sent 阶段，表示发送响应阶段
	ctx, cancel := context.WithCancel(context.Background()) // 保证子携程能退出
	defer cancel()

	// [~WARNING][Done] 该处可能存在内存泄露，通过 cancel context 解决
	// [WARNING] sent 超时的情况没有处理
	go func(ctx context.Context) {
		err := req.svc.call(req.mtype, req.argv, req.replyv)
		select {
		case called <- struct{}{}:
		case <-ctx.Done():
			return
		}

		if err != nil {
			req.h.Error = err.Error()
			s.sendResponse(cc, req.h, invalidRequest, sending)
			sent <- struct{}{}
			return
		}

		s.sendResponse(cc, req.h, req.replyv.Interface(), sending)
		sent <- struct{}{}
	}(ctx)

	if timeout == 0 {
		<-called
		<-sent
	}

	select {
	case <-called:
		<-sent
	case <-time.After(timeout):
		// 调用服务超时
		req.h.Error = fmt.Sprintf("[easy-rpc server] request handle timeout: expect within %s", timeout)
		s.sendResponse(cc, req.h, invalidRequest, sending)
	}
}

/*
服务注册相关
*/

// methodType 包含一个方法的完整信息
type methodType struct {
	method    reflect.Method
	ArgType   reflect.Type
	ReplyType reflect.Type
	numCalls  uint64 // 统计方法调用次数
}

func (m *methodType) NumCalls() uint64 {
	return atomic.LoadUint64(&m.numCalls)
}

func (m *methodType) newArgv() reflect.Value {
	var argv reflect.Value

	if m.ArgType.Kind() == reflect.Ptr {
		// 如果不使用 m.ArgType.Elem(), New() 里的参数将会是 Ptr
		argv = reflect.New(m.ArgType.Elem())
	} else {
		// 此处不加 Elem() 似乎也可
		argv = reflect.New(m.ArgType).Elem()
	}
	return argv
}

func (m *methodType) newReplyv() reflect.Value {
	replyv := reflect.New(m.ReplyType.Elem())

	// 若是 map 或 slice 类型，需要 make 一下
	switch m.ReplyType.Elem().Kind() {
	case reflect.Map:
		replyv.Elem().Set(reflect.MakeMap(m.ReplyType.Elem()))
	case reflect.Slice:
		replyv.Elem().Set(reflect.MakeSlice(m.ReplyType.Elem(), 0, 0))
	}

	return replyv
}

/*
服务本体相关
*/

type service struct {
	name   string                 // 映射的结构体名称
	typ    reflect.Type           // 结构体类型
	rcvr   reflect.Value          // 结构体本身，在调用时需要用它当第 0 个参数
	method map[string]*methodType // 包含的可远程调用方法
}

func newService(rcvr interface{}) *service {
	s := new(service)
	s.typ = reflect.TypeOf(rcvr)
	s.rcvr = reflect.ValueOf(rcvr)
	s.name = reflect.Indirect(s.rcvr).Type().Name()
	if !ast.IsExported(s.name) {
		log.Fatalf("[easy-rpc server] %s is not a valid service name", s.name)
	}

	s.registerMethods()
	return s
}

func (s *service) registerMethods() {
	s.method = make(map[string]*methodType)

	for i := 0; i < s.typ.NumMethod(); i++ {
		method := s.typ.Method(i)
		mType := method.Type

		// 检查远程调用方法的基本规则
		if mType.NumIn() != 3 || mType.NumOut() != 1 {
			continue
		}
		if mType.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
			continue
		}

		argType, replyType := mType.In(1), mType.In(2)
		if !isExportedOrBuiltinType(argType) || !isExportedOrBuiltinType(replyType) {
			continue
		}

		s.method[method.Name] = &methodType{
			method:    method,
			ArgType:   argType,
			ReplyType: replyType,
		}
		log.Printf("[easy-rpc server] register %s.%s successfully.\n", s.name, method.Name)
	}
}

func isExportedOrBuiltinType(t reflect.Type) bool {
	return ast.IsExported(t.Name()) || t.PkgPath() == ""
}

func (s *service) call(m *methodType, argv, replyv reflect.Value) error {
	atomic.AddUint64(&m.numCalls, 1)
	f := m.method.Func
	returnValues := f.Call([]reflect.Value{s.rcvr, argv, replyv})
	if errInter := returnValues[0].Interface(); errInter != nil {
		return errInter.(error)
	}

	return nil
}

/*
支持 HTTP 协议
*/

const (
	connected        = "200 Connected to Easy RPC"
	defaultRPCPath   = "/easyrpc"
	defaultDebugPath = "/debug/easyrpc"
)

func (s *Server) HandleHTTP() {
	http.Handle(defaultRPCPath, s)
	http.Handle(defaultDebugPath, debugHTTP{Server: s})
	log.Println("rpc server debug path:", defaultDebugPath)
}

func HandleHTTP() {
	DefaultServer.HandleHTTP()
}

// 实现 http.Handler
// 只接受 CONNECT 请求，使用 s.ServeConn 接管这个连接
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "CONNECT" {
		w.Header().Set("Contect-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, "405 must CONNECT\n")
		return
	}

	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Print("rpc hijacking ", req.RemoteAddr, ": ", err.Error())
		return
	}

	_, _ = io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")
	s.ServeConn(conn)
}
