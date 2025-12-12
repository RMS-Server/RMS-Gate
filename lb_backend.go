package main

import (
	"context"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	Addr           string
	MaxConnections int

	currentConns atomic.Int32
	failCount    atomic.Int32
	successCount atomic.Int32
	healthy      atomic.Bool
	disabled     atomic.Bool

	latencyWindow []int64
	windowMu      sync.RWMutex
	windowSize    int

	lastCheckTime atomic.Int64

	players   map[string]struct{}
	playersMu sync.RWMutex

	// Trust coefficient for recovery cooldown (0.5 ~ 1.0)
	trustCoeff atomic.Int32 // stored as percentage (50-100)
}

func NewBackend(addr string, maxConns int, windowSize int) *Backend {
	b := &Backend{
		Addr:           addr,
		MaxConnections: maxConns,
		latencyWindow:  make([]int64, 0, windowSize),
		windowSize:     windowSize,
		players:        make(map[string]struct{}),
	}
	b.healthy.Store(true)
	b.trustCoeff.Store(100) // fully trusted initially
	return b
}

func (b *Backend) RecordLatency(latency time.Duration) {
	b.windowMu.Lock()
	defer b.windowMu.Unlock()

	ms := latency.Milliseconds()
	if len(b.latencyWindow) >= b.windowSize {
		b.latencyWindow = b.latencyWindow[1:]
	}
	b.latencyWindow = append(b.latencyWindow, ms)
}

func (b *Backend) AvgLatency() float64 {
	b.windowMu.RLock()
	defer b.windowMu.RUnlock()

	if len(b.latencyWindow) == 0 {
		return 0
	}
	var sum int64
	for _, v := range b.latencyWindow {
		sum += v
	}
	return float64(sum) / float64(len(b.latencyWindow))
}

func (b *Backend) Jitter() float64 {
	b.windowMu.RLock()
	defer b.windowMu.RUnlock()

	if len(b.latencyWindow) < 2 {
		return 0
	}

	var sum int64
	for _, v := range b.latencyWindow {
		sum += v
	}
	avg := float64(sum) / float64(len(b.latencyWindow))

	var variance float64
	for _, v := range b.latencyWindow {
		diff := float64(v) - avg
		variance += diff * diff
	}
	return math.Sqrt(variance / float64(len(b.latencyWindow)))
}

// Trend returns the latency trend: positive = getting worse, negative = improving
// Compares recent 1/4 samples vs older 3/4 samples
func (b *Backend) Trend() float64 {
	b.windowMu.RLock()
	defer b.windowMu.RUnlock()

	n := len(b.latencyWindow)
	if n < 8 {
		return 0
	}

	// Split: recent 1/4 vs older 3/4
	recentStart := n - n/4
	if recentStart < 1 {
		recentStart = 1
	}

	var recentSum, olderSum int64
	recentCount := n - recentStart
	olderCount := recentStart

	for i := 0; i < recentStart; i++ {
		olderSum += b.latencyWindow[i]
	}
	for i := recentStart; i < n; i++ {
		recentSum += b.latencyWindow[i]
	}

	recentAvg := float64(recentSum) / float64(recentCount)
	olderAvg := float64(olderSum) / float64(olderCount)

	if olderAvg == 0 {
		return 0
	}

	// Return percentage change: positive = worse, negative = better
	return (recentAvg - olderAvg) / olderAvg * 100
}

// HealthScore calculates score independently (legacy, used for display only)
func (b *Backend) HealthScore(jitterThreshold float64) int {
	if b.disabled.Load() {
		return 0
	}
	if !b.healthy.Load() {
		return 0
	}

	// Use simple scoring for display
	score := 100

	avg := b.AvgLatency()
	if avg > 0 {
		// Deduct based on latency (higher = worse)
		penalty := int(avg / 5)
		if penalty > 40 {
			penalty = 40
		}
		score -= penalty
	}

	jitter := b.Jitter()
	if jitter > 0 {
		penalty := int(jitter / 2)
		if penalty > 30 {
			penalty = 30
		}
		score -= penalty
	}

	if b.MaxConnections > 0 {
		ratio := float64(b.currentConns.Load()) / float64(b.MaxConnections)
		score -= int(ratio * 20)
	}

	if b.failCount.Load() > 0 {
		score -= 10
	}

	// Apply trust coefficient
	trust := float64(b.trustCoeff.Load()) / 100.0
	score = int(float64(score) * trust)

	if score < 0 {
		score = 0
	}
	return score
}

// RelativeHealthScore calculates score relative to other backends
func (b *Backend) RelativeHealthScore(minLatency, minJitter float64) int {
	if b.disabled.Load() {
		return 0
	}
	if !b.healthy.Load() {
		return 0
	}

	var score float64 = 0

	// 1. Latency score (40 points max) - relative to best
	avg := b.AvgLatency()
	if avg > 0 && minLatency > 0 {
		score += 40 * (minLatency / avg)
	} else if avg == 0 {
		score += 40 // no data = full score
	}

	// 2. Jitter score (30 points max) - relative to best
	jitter := b.Jitter()
	if jitter > 0 && minJitter > 0 {
		score += 30 * (minJitter / jitter)
	} else if jitter == 0 {
		score += 30 // no jitter = full score
	}

	// 3. Connection score (20 points max)
	if b.MaxConnections > 0 {
		ratio := float64(b.currentConns.Load()) / float64(b.MaxConnections)
		score += 20 * (1 - ratio)
	} else {
		score += 20
	}

	// 4. Stability score (10 points max)
	if b.failCount.Load() == 0 {
		score += 10
	}

	// 5. Trend adjustment (-10 to +10)
	trend := b.Trend()
	if trend > 20 {
		// Getting worse by >20%, penalize
		score -= 10
	} else if trend > 10 {
		score -= 5
	} else if trend < -20 {
		// Improving by >20%, bonus
		score += 10
	} else if trend < -10 {
		score += 5
	}

	// 6. Apply trust coefficient (0.5 ~ 1.0)
	trust := float64(b.trustCoeff.Load()) / 100.0
	score = score * trust

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return int(score)
}

func (b *Backend) TrustCoeff() float64 {
	return float64(b.trustCoeff.Load()) / 100.0
}

func (b *Backend) IncreaseTrust() {
	current := b.trustCoeff.Load()
	if current < 100 {
		newVal := current + 10
		if newVal > 100 {
			newVal = 100
		}
		b.trustCoeff.Store(newVal)
	}
}

func (b *Backend) ResetTrust() {
	b.trustCoeff.Store(50) // Start at 50% trust after recovery
}

func (b *Backend) IsAvailable() bool {
	if b.disabled.Load() {
		return false
	}
	if !b.healthy.Load() {
		return false
	}
	if b.MaxConnections > 0 && b.currentConns.Load() >= int32(b.MaxConnections) {
		return false
	}
	return true
}

func (b *Backend) AddPlayer(name string) {
	b.playersMu.Lock()
	b.players[name] = struct{}{}
	b.playersMu.Unlock()
	b.currentConns.Add(1)
}

func (b *Backend) RemovePlayer(name string) {
	b.playersMu.Lock()
	_, exists := b.players[name]
	if exists {
		delete(b.players, name)
	}
	b.playersMu.Unlock()
	if exists {
		b.currentConns.Add(-1)
	}
}

func (b *Backend) GetPlayers() []string {
	b.playersMu.RLock()
	defer b.playersMu.RUnlock()
	players := make([]string, 0, len(b.players))
	for name := range b.players {
		players = append(players, name)
	}
	return players
}

func (b *Backend) CurrentConns() int32 {
	return b.currentConns.Load()
}

func (b *Backend) RecordSuccess() {
	b.failCount.Store(0)
}

// RecordHealthCheckSuccess records a successful health check (used for recovery counting)
func (b *Backend) RecordHealthCheckSuccess() {
	b.failCount.Store(0)
	b.successCount.Add(1)
}

func (b *Backend) SuccessCount() int32 {
	return b.successCount.Load()
}

func (b *Backend) ResetSuccessCount() {
	b.successCount.Store(0)
}

func (b *Backend) RecordFailure() {
	b.failCount.Add(1)
}

// RecordHealthCheckFailure records a failed health check (resets recovery counter)
func (b *Backend) RecordHealthCheckFailure() {
	b.failCount.Add(1)
	b.successCount.Store(0)
}

func (b *Backend) FailCount() int32 {
	return b.failCount.Load()
}

func (b *Backend) SetHealthy(healthy bool) {
	b.healthy.Store(healthy)
}

func (b *Backend) IsHealthy() bool {
	return b.healthy.Load()
}

func (b *Backend) SetDisabled(disabled bool) {
	b.disabled.Store(disabled)
}

func (b *Backend) IsDisabled() bool {
	return b.disabled.Load()
}

func (b *Backend) SetLastCheckTime(t time.Time) {
	b.lastCheckTime.Store(t.UnixMilli())
}

func (b *Backend) LastCheckTime() time.Time {
	return time.UnixMilli(b.lastCheckTime.Load())
}

func (b *Backend) MCPing(timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	latency := time.Since(start)

	// Hard cap total ping time (DNS + dial + status exchange).
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", b.Addr)
	if err != nil {
		return latency, err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	err = MCPingConn(conn, b.Addr, timeout)
	if err != nil {
		return latency, err
	}
	return latency, nil
}

func (b *Backend) Stats() BackendStats {
	return BackendStats{
		Addr:           b.Addr,
		CurrentConns:   b.currentConns.Load(),
		MaxConnections: b.MaxConnections,
		AvgLatency:     b.AvgLatency(),
		Jitter:         b.Jitter(),
		FailCount:      b.failCount.Load(),
		Healthy:        b.healthy.Load(),
		Disabled:       b.disabled.Load(),
		Players:        b.GetPlayers(),
	}
}

type BackendStats struct {
	Addr           string
	CurrentConns   int32
	MaxConnections int
	AvgLatency     float64
	Jitter         float64
	FailCount      int32
	Healthy        bool
	Disabled       bool
	Players        []string
}
