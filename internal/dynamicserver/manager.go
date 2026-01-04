package dynamicserver

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"go.minekube.com/gate/pkg/edition/java/proxy"

	"github.com/RMS-Server/RMS-Gate/internal/loadbalancer"
	"github.com/RMS-Server/RMS-Gate/internal/mcsmanager"
	"github.com/RMS-Server/RMS-Gate/internal/minecraft"
)

type Config struct {
	ServerUUIDMap              map[string]string
	AutoStartServers           []string
	StartupTimeoutSeconds      int
	PollIntervalSeconds        int
	ConnectivityTimeoutSeconds int
	IdleShutdownSeconds        int
	MsgStarting                string
	MsgStartupTimeout          string
}

type ShutdownConfig struct {
	protectionEndTime atomic.Int64
	enabled           atomic.Bool
}

func NewShutdownConfig(enabled bool) *ShutdownConfig {
	cfg := &ShutdownConfig{}
	cfg.enabled.Store(enabled)
	return cfg
}

func (s *ShutdownConfig) IsInProtectionPeriod() bool {
	return time.Now().UnixMilli() < s.protectionEndTime.Load()
}

func (s *ShutdownConfig) SetProtectionEndTime(timestamp int64) {
	s.protectionEndTime.Store(timestamp)
}

func (s *ShutdownConfig) ClearProtection() {
	s.protectionEndTime.Store(0)
}

func (s *ShutdownConfig) IsEnabled() bool {
	return s.enabled.Load()
}

func (s *ShutdownConfig) SetEnabled(v bool) {
	s.enabled.Store(v)
}

type startingServer struct {
	done   chan struct{}
	result bool
}

type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    logr.Logger
	proxy  *proxy.Proxy
	mcs    *mcsmanager.Client
	cfg    *Config

	mu              sync.Mutex
	startingServers map[string]*startingServer
	shutdownTimers  map[string]*time.Timer
	serverConfigs   map[string]*ShutdownConfig
}

func NewManager(ctx context.Context, log logr.Logger, p *proxy.Proxy, mcs *mcsmanager.Client, cfg *Config) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{
		ctx:             ctx,
		cancel:          cancel,
		log:             log.WithName("dynamic-server"),
		proxy:           p,
		mcs:             mcs,
		cfg:             cfg,
		startingServers: make(map[string]*startingServer),
		shutdownTimers:  make(map[string]*time.Timer),
		serverConfigs:   make(map[string]*ShutdownConfig),
	}

	m.log.Info("DynamicServerManager initialized", "autoStart", cfg.AutoStartServers)
	go m.periodicIdleCheck()
	return m
}

func (m *Manager) IsAutoStartServer(name string) bool {
	for _, s := range m.cfg.AutoStartServers {
		if s == name {
			return true
		}
	}
	return false
}

func (m *Manager) EnsureServerRunning(serverName string) bool {
	m.mu.Lock()
	if s, ok := m.startingServers[serverName]; ok {
		m.mu.Unlock()
		<-s.done
		return s.result
	}

	s := &startingServer{done: make(chan struct{})}
	m.startingServers[serverName] = s
	m.mu.Unlock()

	defer func() {
		close(s.done)
		m.mu.Lock()
		delete(m.startingServers, serverName)
		m.mu.Unlock()
	}()

	m.log.Info("Starting server", "server", serverName)

	instanceUUID, ok := m.cfg.ServerUUIDMap[serverName]
	if !ok {
		m.log.Error(nil, "No MCSManager UUID configured for server", "server", serverName)
		s.result = false
		return false
	}

	started, err := m.mcs.StartInstance(m.ctx, instanceUUID)
	if err != nil || !started {
		m.log.Error(err, "Failed to send start command", "server", serverName)
		s.result = false
		return false
	}

	if !m.waitForServerReady(serverName, instanceUUID) {
		m.log.Error(nil, "Server failed to start", "server", serverName)
		s.result = false
		return false
	}

	m.log.Info("Server is now running", "server", serverName)
	s.result = true
	return true
}

func (m *Manager) waitForServerReady(serverName, instanceUUID string) bool {
	pollInterval := time.Duration(m.cfg.PollIntervalSeconds) * time.Second
	maxAttempts := m.cfg.StartupTimeoutSeconds / m.cfg.PollIntervalSeconds

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-m.ctx.Done():
			return false
		default:
		}

		status, err := m.mcs.GetInstanceStatus(m.ctx, instanceUUID)
		if err != nil {
			m.log.V(1).Info("Status check error, retrying", "server", serverName, "error", err)
			time.Sleep(pollInterval)
			continue
		}

		m.log.V(1).Info("Server status check", "server", serverName, "attempt", attempt+1, "status", status)

		if status == 3 {
			m.log.Info("Server process running, checking connectivity", "server", serverName)
			return m.checkServerConnectivity(serverName)
		} else if status == 0 || status == 1 || status == 2 {
			time.Sleep(pollInterval)
		} else {
			m.log.Error(nil, "Server entered error state", "server", serverName, "status", status)
			return false
		}
	}

	m.log.Error(nil, "Server startup timed out", "server", serverName)
	return false
}

func (m *Manager) checkServerConnectivity(serverName string) bool {
	server := m.proxy.Server(serverName)
	if server == nil {
		m.log.Error(nil, "Server not registered in proxy", "server", serverName)
		return false
	}

	pollInterval := time.Duration(m.cfg.PollIntervalSeconds) * time.Second
	maxAttempts := m.cfg.ConnectivityTimeoutSeconds / m.cfg.PollIntervalSeconds

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-m.ctx.Done():
			return false
		default:
		}

		// Check connectivity - try all backends if load-balanced
		if m.checkAnyBackendReachable(server, serverName) {
			return true
		}

		m.log.V(1).Info("Server not yet accepting connections", "server", serverName, "attempt", attempt+1)
		time.Sleep(pollInterval)
	}

	m.log.Error(nil, "Server connectivity check timed out", "server", serverName)
	return false
}

func (m *Manager) checkAnyBackendReachable(server proxy.RegisteredServer, serverName string) bool {
	serverInfo := server.ServerInfo()

	// Check if this is a load-balanced server with multiple backends
	type backendProvider interface {
		Backends() []*loadbalancer.Backend
	}

	if lbInfo, ok := serverInfo.(backendProvider); ok {
		// Load-balanced server - check all backends
		backends := lbInfo.Backends()
		for _, backend := range backends {
			addr, err := net.ResolveTCPAddr("tcp", backend.Addr)
			if err != nil {
				m.log.V(1).Info("Failed to resolve backend address", "backend", backend.Addr, "error", err)
				continue
			}

			if minecraft.MCPing(addr, 3*time.Second) == nil {
				m.log.Info("Server is accepting connections (MC ping success)",
					"server", serverName, "backend", backend.Addr)
				return true
			}
		}
		return false
	}

	// Regular server - check single address
	addr := serverInfo.Addr()
	if minecraft.MCPing(addr, 3*time.Second) == nil {
		m.log.Info("Server is accepting connections (MC ping success)",
			"server", serverName, "addr", addr.String())
		return true
	}
	return false
}

func (m *Manager) IsServerStarting(serverName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.startingServers[serverName]
	return ok
}

func (m *Manager) periodicIdleCheck() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	m.log.Info("Started periodic idle check (every 10 seconds)")

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.checkAllAutoStartServersIdle()
		}
	}
}

func (m *Manager) checkAllAutoStartServersIdle() {
	for _, serverName := range m.cfg.AutoStartServers {
		m.mu.Lock()
		cfg := m.serverConfigs[serverName]
		m.mu.Unlock()

		if cfg != nil && cfg.IsInProtectionPeriod() {
			m.log.V(1).Info("Server is in protection period, skipping idle check", "server", serverName)
			continue
		}

		server := m.proxy.Server(serverName)
		if server == nil {
			continue
		}

		playerCount := server.Players().Len()
		if playerCount == 0 {
			m.scheduleShutdown(serverName)
		} else {
			m.cancelShutdown(serverName)
		}
	}
}

func (m *Manager) scheduleShutdown(serverName string) {
	if !m.IsAutoShutdownEnabled(serverName) {
		m.log.V(1).Info("Auto-shutdown disabled for server, skipping", "server", serverName)
		return
	}

	instanceUUID, ok := m.cfg.ServerUUIDMap[serverName]
	if !ok {
		m.log.V(1).Info("No UUID configured for server, cannot schedule shutdown", "server", serverName)
		return
	}

	m.mu.Lock()
	if _, ok := m.shutdownTimers[serverName]; ok {
		m.mu.Unlock()
		m.log.V(1).Info("Shutdown already scheduled", "server", serverName)
		return
	}
	m.mu.Unlock()

	status, err := m.mcs.GetInstanceStatus(m.ctx, instanceUUID)
	if err != nil || status != 3 {
		m.log.V(1).Info("Server is not running, skipping idle shutdown schedule", "server", serverName, "status", status)
		return
	}

	m.log.Info("Scheduling idle shutdown", "server", serverName, "seconds", m.cfg.IdleShutdownSeconds)

	timer := time.AfterFunc(time.Duration(m.cfg.IdleShutdownSeconds)*time.Second, func() {
		m.mu.Lock()
		delete(m.shutdownTimers, serverName)
		m.mu.Unlock()

		server := m.proxy.Server(serverName)
		if server == nil {
			return
		}

		playerCount := server.Players().Len()
		if playerCount > 0 {
			m.log.Info("Shutdown cancelled - players online", "server", serverName, "players", playerCount)
			return
		}

		m.log.Info("Server idle, sending stop command", "server", serverName, "idleSeconds", m.cfg.IdleShutdownSeconds)

		stopped, err := m.mcs.StopInstance(m.ctx, instanceUUID)
		if err != nil || !stopped {
			m.log.Error(err, "Failed to stop server", "server", serverName)
		} else {
			m.log.Info("Successfully stopped server", "server", serverName)
		}
	})

	m.mu.Lock()
	m.shutdownTimers[serverName] = timer
	m.mu.Unlock()
}

func (m *Manager) cancelShutdown(serverName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if timer, ok := m.shutdownTimers[serverName]; ok {
		timer.Stop()
		delete(m.shutdownTimers, serverName)
		m.log.Info("Cancelled idle shutdown", "server", serverName)
	}
}

func (m *Manager) SetShutdownDelay(serverName string, delaySeconds int) {
	m.mu.Lock()
	cfg, ok := m.serverConfigs[serverName]
	if !ok {
		cfg = NewShutdownConfig(true)
		m.serverConfigs[serverName] = cfg
	}
	m.mu.Unlock()

	protectionEndTime := time.Now().UnixMilli() + int64(delaySeconds)*1000
	cfg.SetProtectionEndTime(protectionEndTime)

	m.cancelShutdown(serverName)

	m.log.Info("Set protection period", "server", serverName, "seconds", delaySeconds)
}

func (m *Manager) ClearProtectionPeriod(serverName string) {
	m.mu.Lock()
	cfg := m.serverConfigs[serverName]
	m.mu.Unlock()

	if cfg != nil {
		cfg.ClearProtection()
		m.log.Info("Cleared protection period", "server", serverName)
	}
}

func (m *Manager) SetAutoShutdownEnabled(serverName string, enabled bool) {
	m.mu.Lock()
	cfg, ok := m.serverConfigs[serverName]
	if !ok {
		cfg = NewShutdownConfig(true)
		m.serverConfigs[serverName] = cfg
	}
	m.mu.Unlock()

	cfg.SetEnabled(enabled)

	if !enabled {
		m.cancelShutdown(serverName)
		cfg.ClearProtection()
		m.log.Info("Auto-shutdown disabled", "server", serverName)
	} else {
		m.log.Info("Auto-shutdown enabled", "server", serverName)
	}
}

func (m *Manager) IsAutoShutdownEnabled(serverName string) bool {
	m.mu.Lock()
	cfg := m.serverConfigs[serverName]
	m.mu.Unlock()

	return cfg == nil || cfg.IsEnabled()
}

func (m *Manager) Shutdown() {
	m.log.Info("Shutting down DynamicServerManager")
	m.cancel()

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, timer := range m.shutdownTimers {
		timer.Stop()
	}
	m.shutdownTimers = make(map[string]*time.Timer)
}

// GetServerAddr returns the server address for external use
func (m *Manager) GetServerAddr(serverName string) net.Addr {
	server := m.proxy.Server(serverName)
	if server == nil {
		return nil
	}
	return server.ServerInfo().Addr()
}

// GetConfig returns the dynamic server config
func (m *Manager) GetConfig() *Config {
	return m.cfg
}
