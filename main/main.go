package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	easyrpc "github.com/Congregalis/easy-rpc"
	"github.com/Congregalis/easy-rpc/registry"
	"github.com/Congregalis/easy-rpc/xclient"
)

/*
以下为服务端代码
*/

func startServer(registryAddr string, wg *sync.WaitGroup) {
	var calc Calc
	l, _ := net.Listen("tcp", ":0")
	server := easyrpc.NewServer()
	_ = server.Register(&calc)
	registry.Heartbeat(registryAddr, "tcp@"+l.Addr().String(), 0)
	wg.Done()
	server.Accept(l)
}

/*
以下为客户端代码
*/

func doCall(xc *xclient.XClient, ctx context.Context, typ, serviceMethod string, args *Args) {
	var reply int
	var err error
	switch typ {
	case "call":
		err = xc.Call(ctx, serviceMethod, args, &reply)
	case "broadcast":
		err = xc.Broadcast(ctx, serviceMethod, args, &reply)
	}
	if err != nil {
		log.Printf("%s %s error: %v", typ, serviceMethod, err)
	} else {
		log.Printf("%s %s success: %d + %d = %d", typ, serviceMethod, args.Num1, args.Num2, reply)
	}
}

func call(d *xclient.RegistryDiscovery) {
	xc := xclient.NewXClient(d, xclient.RandomSelect, nil)
	defer xc.Close()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			doCall(xc, context.Background(), "call", "Calc.Plus", &Args{i, i * i})
		}(i)
	}
	wg.Wait()
}

func broadcast(d *xclient.RegistryDiscovery) {
	xc := xclient.NewXClient(d, xclient.RandomSelect, nil)
	defer xc.Close()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			doCall(xc, context.Background(), "broadcast", "Calc.Plus", &Args{i, i * i})

			ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
			defer cancel()
			doCall(xc, ctx, "broadcast", "Calc.Sleep", &Args{i, i * i})
		}(i)
	}
	wg.Wait()
}

/*
以下为注册中心代码
*/

func startRegistry(wg *sync.WaitGroup) {
	l, _ := net.Listen("tcp", ":9999")
	registry.HandleHTTP()
	wg.Done()
	_ = http.Serve(l, nil)
}

func main() {
	log.SetFlags(0)
	// 启动注册中心
	registryAddr := "http://localhost:9999/easyrpc/registry"
	var wg sync.WaitGroup
	wg.Add(1)
	go startRegistry(&wg)
	wg.Wait()

	// 启动服务
	time.Sleep(time.Second)
	wg.Add(2)
	go startServer(registryAddr, &wg)
	go startServer(registryAddr, &wg)
	wg.Wait()

	// 客户端发起请求
	time.Sleep(time.Second)
	d := xclient.NewRegistryDiscovery(registryAddr, 0)
	call(d)
	broadcast(d)
}
