package registry

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

/*
注册中心
*/

type Registry struct {
	timeout time.Duration
	mu      sync.Mutex
	servers map[string]*ServerItem
}

type ServerItem struct {
	Addr     string
	lastBeat time.Time // 上一次响应的时间
}

const (
	defaultTimeout = 5 * time.Minute
	defaultPath    = "/easyrpc/registry"
)

func New(timeout time.Duration) *Registry {
	return &Registry{
		servers: make(map[string]*ServerItem),
		timeout: timeout,
	}
}

var DefaultRegister = New(defaultTimeout)

// 添加服务实例，若该服务实例已存在则更新其上一次响应时间
func (r *Registry) putServer(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := r.servers[addr]
	if s == nil {
		r.servers[addr] = &ServerItem{addr, time.Now()}
	} else {
		s.lastBeat = time.Now()
	}
}

// 返回可用的服务列表，若存在超时服务则从列表中移除
func (r *Registry) aliveServers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var alive []string
	for addr, s := range r.servers {
		if r.timeout == 0 || s.lastBeat.Add(r.timeout).After(time.Now()) {
			alive = append(alive, addr)
		} else {
			delete(r.servers, addr)
		}
	}

	sort.Strings(alive) // 按字典序排序，也可以按 lastBeat 排序
	return alive
}

/*
使用 HTTP 协议提供服务
*/

func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case "GET":
		w.Header().Set("X-Easyrpc-Servers", strings.Join(r.aliveServers(), ","))
	case "POST":
		addr := req.Header.Get("X-Easyrpc-Server")
		if addr == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		r.putServer(addr)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *Registry) HandleHTTP(registryPath string) {
	http.Handle(registryPath, r)
	log.Println("rpc registry path:", registryPath)
}

func HandleHTTP() {
	DefaultRegister.HandleHTTP(defaultPath)
}

/*
心跳机制
*/

// 服务启动时应调用的方法、
// 每隔 duration 时间，addr 向 registry 发送心跳证明服务正常运行
func Heartbeat(registry, addr string, duration time.Duration) {
	if duration == 0 {
		// duration 为 0 说明要设置为默认值，默认值为超时时间减一分钟
		duration = defaultTimeout - time.Duration(1)*time.Minute
	}

	err := sendHeartbeat(registry, addr)
	go func() {
		t := time.NewTicker(duration)
		for err == nil {
			<-t.C
			err = sendHeartbeat(registry, addr)
		}
	}()
}

func sendHeartbeat(registry, addr string) error {
	log.Println(addr, "send heart beat to registry", registry)
	httpClient := &http.Client{}
	req, _ := http.NewRequest("POST", registry, nil)
	req.Header.Set("X-Easyrpc-Server", addr)

	if _, err := httpClient.Do(req); err != nil {
		log.Println("[easy-rpc server]: heart beat err:", err)
		return err
	}
	return nil
}
