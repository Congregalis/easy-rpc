package xclient

import (
	"log"
	"net/http"
	"strings"
	"time"
)

/*
使用服务中心的服务发现模块
*/

type RegistryDiscovery struct {
	*MultiServersDiscovery

	registry   string
	timeout    time.Duration // 服务列表的过期时间
	lastUpdate time.Time     // 上一次更新服务列表的时间
}

const (
	defaultUpdateTimeout = 10 * time.Second
)

func NewRegistryDiscovery(registryAddr string, timeout time.Duration) *RegistryDiscovery {
	if timeout == 0 {
		// 默认值
		timeout = defaultUpdateTimeout
	}

	return &RegistryDiscovery{
		MultiServersDiscovery: NewMultiServersDiscovery(make([]string, 0)),
		registry:              registryAddr,
		timeout:               timeout,
	}
}

/*
以下重写父类方法
*/

func (d *RegistryDiscovery) Update(servers []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.servers = servers
	d.lastUpdate = time.Now()
	return nil
}

func (d *RegistryDiscovery) Refresh() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// 若还在有效期内，则不用更新，节省资源
	if d.lastUpdate.Add(d.timeout).After(time.Now()) {
		return nil
	}

	log.Println("[easy-rpc registry] refresh servers from registry", d.registry)
	resp, err := http.Get(d.registry)
	if err != nil {
		log.Println("rpc registry refresh err:", err)
		return err
	}

	// 更新时替换整个 d.servers，[TODO] 可以考虑动态替换节省内存开销
	servers := strings.Split(resp.Header.Get("X-Easyrpc-Servers"), ",")
	d.servers = make([]string, 0, len(servers))
	for _, server := range servers {
		if strings.TrimSpace(server) != "" {
			d.servers = append(d.servers, strings.TrimSpace(server))
		}
	}

	d.lastUpdate = time.Now()
	return nil
}

func (d *RegistryDiscovery) Get(mode SelectMode) (string, error) {
	if err := d.Refresh(); err != nil {
		return "", err
	}

	return d.MultiServersDiscovery.Get(mode)
}

func (d *RegistryDiscovery) GetAll() ([]string, error) {
	if err := d.Refresh(); err != nil {
		return nil, err
	}

	return d.MultiServersDiscovery.GetAll()
}
