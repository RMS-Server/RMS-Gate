package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.minekube.com/gate/pkg/edition/java/proxy"
)

type LoadBalancer struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    logr.Logger
	proxy  *proxy.Proxy
	cfg    *LoadBalancerConfig

	servers map[string]*LoadBalancedServerInfo
	mu      sync.RWMutex

	history *HistoryManager
	stopCh  chan struct{}
}

func NewLoadBalancer(ctx context.Context, log logr.Logger, p *proxy.Proxy, cfg *LoadBalancerConfig, dataDir string) *LoadBalancer {
	ctx, cancel := context.WithCancel(ctx)
	lb := &LoadBalancer{
		ctx:     ctx,
		cancel:  cancel,
		log:     log.WithName("load-balancer"),
		proxy:   p,
		cfg:     cfg,
		servers: make(map[string]*LoadBalancedServerInfo),
		history: NewHistoryManager(dataDir),
		stopCh:  make(chan struct{}),
	}
	return lb
}

func (lb *LoadBalancer) Start() error {
	if lb.cfg == nil || !lb.cfg.Enabled {
		lb.log.Info("Load balancer disabled")
		return nil
	}

	for name, serverCfg := range lb.cfg.Servers {
		if err := lb.registerServer(name, serverCfg); err != nil {
			lb.log.Error(err, "Failed to register load balanced server", "server", name)
			continue
		}
		lb.log.Info("Registered load balanced server", "server", name, "backends", len(serverCfg.Backends), "strategy", serverCfg.Strategy)
	}

	go lb.healthCheckLoop()

	// Start auto-save for history (every 5 minutes)
	lb.history.StartAutoSave(5*time.Minute, lb.stopCh)

	lb.log.Info("Load balancer started", "servers", len(lb.servers))
	return nil
}

func (lb *LoadBalancer) registerServer(name string, cfg *LBServerConfig) error {
	backends := make([]*Backend, 0, len(cfg.Backends))
	for _, bcfg := range cfg.Backends {
		backend := NewBackend(bcfg.Addr, bcfg.MaxConnections, lb.cfg.HealthCheck.WindowSize)
		backends = append(backends, backend)
	}

	strategy := GetStrategy(cfg.Strategy)
	dialTimeout := time.Duration(lb.cfg.HealthCheck.DialTimeoutSeconds) * time.Second
	if dialTimeout == 0 {
		dialTimeout = 5 * time.Second
	}

	serverInfo := NewLoadBalancedServerInfo(
		name,
		backends,
		strategy,
		lb.cfg.HealthCheck.JitterThreshold,
		dialTimeout,
		lb.cfg.HealthCheck.UnhealthyAfterFailures,
		lb.history,
	)

	// Unregister existing server with the same name (from Gate config)
	if existing := lb.proxy.Server(name); existing != nil {
		lb.proxy.Unregister(existing.ServerInfo())
		lb.log.Info("Unregistered existing server for load balancing", "server", name)
	}

	_, err := lb.proxy.Register(serverInfo)
	if err != nil {
		return err
	}

	lb.mu.Lock()
	lb.servers[name] = serverInfo
	lb.mu.Unlock()

	return nil
}

func (lb *LoadBalancer) healthCheckLoop() {
	defer func() {
		if r := recover(); r != nil {
			lb.log.Error(fmt.Errorf("panic: %v", r), "Health check loop panicked, restarting")
			go lb.healthCheckLoop()
		}
	}()

	interval := time.Duration(lb.cfg.HealthCheck.IntervalSeconds) * time.Second
	if interval == 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lb.log.Info("Health check loop started", "interval", interval)

	for {
		select {
		case <-lb.ctx.Done():
			return
		case <-ticker.C:
			lb.checkAllBackends()
		}
	}
}

func (lb *LoadBalancer) checkAllBackends() {
	lb.mu.RLock()
	servers := make([]*LoadBalancedServerInfo, 0, len(lb.servers))
	for _, s := range lb.servers {
		servers = append(servers, s)
	}
	lb.mu.RUnlock()

	timeout := time.Duration(lb.cfg.HealthCheck.DialTimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 3 * time.Second
	}

	for _, server := range servers {
		for _, backend := range server.Backends() {
			if backend.IsDisabled() {
				continue
			}

			latency, err := backend.MCPing(timeout)
			backend.SetLastCheckTime(time.Now())

			if err != nil {
				backend.RecordHealthCheckFailure()
				if backend.FailCount() >= int32(lb.cfg.HealthCheck.UnhealthyAfterFailures) {
					if backend.IsHealthy() {
						backend.SetHealthy(false)
						lb.log.Info("Backend marked unhealthy",
							"server", server.Name(),
							"backend", backend.Addr,
							"failCount", backend.FailCount(),
							"error", err)
					}
				}
			} else {
				backend.RecordLatency(latency)
				jitter := backend.Jitter()
				lb.history.Record(backend.Addr, float64(latency.Milliseconds()), jitter)

				wasUnhealthy := !backend.IsHealthy()
				backend.RecordHealthCheckSuccess()
				if wasUnhealthy {
					if backend.SuccessCount() >= int32(lb.cfg.HealthCheck.HealthyAfterSuccesses) {
						backend.SetHealthy(true)
						backend.ResetTrust()
						backend.ResetSuccessCount()
						lb.log.Info("Backend recovered",
							"server", server.Name(),
							"backend", backend.Addr,
							"latency", latency,
							"trust", backend.TrustCoeff(),
							"requiredSuccesses", lb.cfg.HealthCheck.HealthyAfterSuccesses)
					}
				} else {
					backend.IncreaseTrust()
				}
			}
		}
	}
}

func (lb *LoadBalancer) GetServer(name string) *LoadBalancedServerInfo {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	return lb.servers[name]
}

func (lb *LoadBalancer) GetAllServers() map[string]*LoadBalancedServerInfo {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	result := make(map[string]*LoadBalancedServerInfo, len(lb.servers))
	for k, v := range lb.servers {
		result[k] = v
	}
	return result
}

func (lb *LoadBalancer) DisableBackend(serverName, backendAddr string) bool {
	server := lb.GetServer(serverName)
	if server == nil {
		return false
	}

	for _, b := range server.Backends() {
		if b.Addr == backendAddr {
			b.SetDisabled(true)
			lb.log.Info("Backend disabled", "server", serverName, "backend", backendAddr)
			return true
		}
	}
	return false
}

func (lb *LoadBalancer) EnableBackend(serverName, backendAddr string) bool {
	server := lb.GetServer(serverName)
	if server == nil {
		return false
	}

	for _, b := range server.Backends() {
		if b.Addr == backendAddr {
			b.SetDisabled(false)
			lb.log.Info("Backend enabled", "server", serverName, "backend", backendAddr)
			return true
		}
	}
	return false
}

func (lb *LoadBalancer) GetServerStats(serverName string) []BackendStats {
	server := lb.GetServer(serverName)
	if server == nil {
		return nil
	}

	stats := make([]BackendStats, 0, len(server.Backends()))
	for _, b := range server.Backends() {
		stats = append(stats, b.Stats())
	}
	return stats
}

func (lb *LoadBalancer) Shutdown() {
	lb.log.Info("Shutting down load balancer")
	close(lb.stopCh)
	lb.cancel()
}

func (lb *LoadBalancer) History() *HistoryManager {
	return lb.history
}
