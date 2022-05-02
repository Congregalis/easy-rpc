package xclient

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"time"
)

/*
基础的服务发现模块
*/

type SelectMode int

const (
	RandomSelect SelectMode = iota
	RoundRobinSelect
	WeightRoundRobinSelect // TODO
	ConsistentHashSelect   // TODO
)

type Discovery interface {
	Refresh() error                      // 从注册中心更新服务列表
	Update(servers []string) error       // 手动更新服务列表
	Get(mode SelectMode) (string, error) // 根据负载均衡策略，选择一个服务实例
	GetAll() ([]string, error)           // 返回所有的服务实例
}

type MultiServersDiscovery struct {
	r       *rand.Rand // 用于 RandomSelect
	index   int        // 用于 RoundRobinSelect，记录目前轮询到的位置
	mu      sync.RWMutex
	servers []string
}

func NewMultiServersDiscovery(servers []string) *MultiServersDiscovery {
	d := &MultiServersDiscovery{
		r:       rand.New(rand.NewSource(time.Now().Unix())),
		servers: servers,
	}

	// 初始化时随机设定一个值
	d.index = d.r.Intn(math.MaxInt32 - 1)
	return d
}

var _ Discovery = &MultiServersDiscovery{}

// 由子类来实现刷新
func (d *MultiServersDiscovery) Refresh() error {
	return nil
}

func (d *MultiServersDiscovery) Update(servers []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.servers = servers
	return nil
}

func (d *MultiServersDiscovery) Get(mode SelectMode) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	n := len(d.servers)
	if n == 0 {
		return "", errors.New("[rpc discovery] no available servers")
	}

	switch mode {
	case RandomSelect:
		return d.servers[d.r.Intn(n)], nil
	case RoundRobinSelect:
		s := d.servers[d.index%n]
		d.index = (d.index + 1) % n
		return s, nil
	default:
		return "", errors.New("[rpc discovery] not supported select mode")
	}
}

func (d *MultiServersDiscovery) GetAll() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	servers := make([]string, len(d.servers))
	copy(servers, d.servers)
	return servers, nil
}
