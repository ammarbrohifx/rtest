package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"
)

var ErrNoActiveInstances = errors.New("No active instances")

type AppConfig struct {
	name        string
	command     string
	healthcheck string
	stopSignal  int
	timeout     int

	internalHost string
	externalHost string
	externalPort uint32
}

type App struct {
	config *AppConfig

	instances          []*Instance
	activeInstance     *Instance
	activeInstanceLock sync.Mutex

	rp       *httputil.ReverseProxy
	portPool *PortPool

	instanceId uint32
}

func NewApp(config *AppConfig, portPool *PortPool) *App {
	app := &App{
		config: config,

		instances: make([]*Instance, 0, 3),

		portPool: portPool,
	}

	app.rp = &httputil.ReverseProxy{Director: func(req *http.Request) {}}

	app.startInstanceUpdater()

	return app
}

func (a *App) startInstanceUpdater() {
	ticker := time.NewTicker(time.Second)

	go func() {
		for {
			for _, instance := range a.instances {
				status := instance.UpdateStatus()
				if status == InstanceStatusServing && instance != a.activeInstance {
					a.activeInstanceLock.Lock()
					currentActive := a.activeInstance
					a.activeInstance = instance
					a.activeInstanceLock.Unlock()

					if currentActive != nil {
						currentActive.Stop()
					}
				} else if status == InstanceStatusExited {
					a.activeInstance = nil
				}
			}
			a.Report()
			<-ticker.C
		}
	}()
}

func (a *App) reserveInstance() (*Instance, error) {
	a.activeInstanceLock.Lock()
	defer a.activeInstanceLock.Unlock()

	if a.activeInstance == nil {
		return nil, ErrNoActiveInstances
	}

	a.activeInstance.Serve()

	return a.activeInstance, nil
}

func (a *App) StartNewInstance() error {
	for _, instance := range a.instances {
		if instance.status == InstanceStatusStarting {
			instance.Stop()
		}
	}

	newInstance, err := NewInstance(a, atomic.AddUint32(&a.instanceId, 1))
	if err != nil {
		return err
	}

	a.instances = append(a.instances, newInstance)
	return nil
}

func (a *App) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	instance, err := a.reserveInstance()

	defer func() {
		if err := recover(); err != nil {
			if err == ErrNoActiveInstances {
				log.Print(err)
				rw.WriteHeader(503)
				req.Body.Close()
			} else {
				log.Print(err)
			}
		}

		if instance != nil {
			instance.Done()
		}
	}()

	if err != nil {
		panic(err)
	}

	req.URL.Scheme = "http"
	req.URL.Host = instance.Hostname()

	host, _, _ := net.SplitHostPort(req.RemoteAddr) //TODO parse real real ip, add fwd for
	req.Header.Add("X-Real-IP", host)

	a.rp.ServeHTTP(rw, req)
}

func (a *App) Hostname() string {
	return fmt.Sprintf("%s:%d", a.config.externalHost, a.config.externalPort)
}

func (a *App) ListenAndServe() {
	http.ListenAndServe(a.Hostname(), a)
}

func (a *App) Report() {
	fmt.Printf("[%s/%s]\n", a.config.name, a.Hostname())
	for _, instance := range a.instances {
		if instance == a.activeInstance {
			fmt.Print(" * ")
		} else {
			fmt.Print("   ")
		}
		fmt.Printf("%d/%s ", instance.id, instance.Hostname())
		switch instance.status {
		case InstanceStatusServing:
			fmt.Print("serving ")
		case InstanceStatusStarting:
			fmt.Print("starting")
		case InstanceStatusStopping:
			fmt.Print("stopping")
		case InstanceStatusStopped:
			fmt.Print("stopped ")
		case InstanceStatusFailed:
			fmt.Print("failed  ")
		case InstanceStatusExited:
			fmt.Print("exited  ")
		}
		fmt.Printf(" %s", time.Since(instance.lastChange)/time.Second*time.Second)
		fmt.Println()
	}
}
