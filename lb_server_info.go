package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"go.minekube.com/gate/pkg/edition/java/proxy"
)

type LoadBalancedServerInfo struct {
	name            string
	backends        []*Backend
	strategy        Strategy
	jitterThreshold float64
	dialTimeout     time.Duration

	defaultAddr net.Addr
	history     *HistoryManager
}

func NewLoadBalancedServerInfo(
	name string,
	backends []*Backend,
	strategy Strategy,
	jitterThreshold float64,
	dialTimeout time.Duration,
	history *HistoryManager,
) *LoadBalancedServerInfo {
	var defaultAddr net.Addr
	if len(backends) > 0 {
		addr, _ := net.ResolveTCPAddr("tcp", backends[0].Addr)
		defaultAddr = addr
	}

	return &LoadBalancedServerInfo{
		name:            name,
		backends:        backends,
		strategy:        strategy,
		jitterThreshold: jitterThreshold,
		dialTimeout:     dialTimeout,
		defaultAddr:     defaultAddr,
		history:         history,
	}
}

func (s *LoadBalancedServerInfo) Name() string {
	return s.name
}

func (s *LoadBalancedServerInfo) Addr() net.Addr {
	return s.defaultAddr
}

func (s *LoadBalancedServerInfo) Dial(ctx context.Context, player proxy.Player) (net.Conn, error) {
	backend := s.strategy.Select(s.backends, s.jitterThreshold, s.history)
	if backend == nil {
		return nil, fmt.Errorf("no available backend for server %s", s.name)
	}

	start := time.Now()

	dialCtx, cancel := context.WithTimeout(ctx, s.dialTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", backend.Addr)

	latency := time.Since(start)

	if err != nil {
		backend.RecordFailure()
		backend.RecordLatency(latency)
		return nil, fmt.Errorf("failed to connect to backend %s: %w", backend.Addr, err)
	}

	playerName := ""
	if player != nil {
		playerName = player.Username()
	}

	backend.RecordSuccess()
	backend.RecordLatency(latency)
	backend.AddPlayer(playerName)

	return &trackedConn{
		Conn:       conn,
		backend:    backend,
		playerName: playerName,
	}, nil
}

func (s *LoadBalancedServerInfo) Backends() []*Backend {
	return s.backends
}

func (s *LoadBalancedServerInfo) Strategy() Strategy {
	return s.strategy
}

type trackedConn struct {
	net.Conn
	backend    *Backend
	playerName string
	closeOnce  sync.Once
}

func (c *trackedConn) Close() error {
	c.closeOnce.Do(func() {
		c.backend.RemovePlayer(c.playerName)
	})
	return c.Conn.Close()
}
