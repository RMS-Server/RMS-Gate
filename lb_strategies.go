package main

import (
	"crypto/rand"
	"math/big"
	"sync/atomic"
)

type Strategy interface {
	Select(backends []*Backend, jitterThreshold float64, history *HistoryManager) *Backend
	Name() string
}

type RoundRobinStrategy struct {
	counter atomic.Uint64
}

func (s *RoundRobinStrategy) Name() string {
	return "round-robin"
}

func (s *RoundRobinStrategy) Select(backends []*Backend, jitterThreshold float64, history *HistoryManager) *Backend {
	available := filterAvailable(backends)
	if len(available) == 0 {
		return nil
	}

	idx := s.counter.Add(1) % uint64(len(available))
	return available[idx]
}

type LeastConnectionsStrategy struct{}

func (s *LeastConnectionsStrategy) Name() string {
	return "least-connections"
}

func (s *LeastConnectionsStrategy) Select(backends []*Backend, jitterThreshold float64, history *HistoryManager) *Backend {
	available := filterAvailable(backends)
	if len(available) == 0 {
		return nil
	}

	var best *Backend
	var minConns int32 = -1

	for _, b := range available {
		conns := b.CurrentConns()
		if minConns < 0 || conns < minConns {
			minConns = conns
			best = b
		}
	}
	return best
}

type HealthScoreStrategy struct{}

func (s *HealthScoreStrategy) Name() string {
	return "health-score"
}

func (s *HealthScoreStrategy) Select(backends []*Backend, jitterThreshold float64, history *HistoryManager) *Backend {
	available := filterAvailable(backends)
	if len(available) == 0 {
		return nil
	}

	// Find minimum latency and jitter across all available backends
	var minLatency, minJitter float64 = -1, -1
	for _, b := range available {
		lat := b.AvgLatency()
		jit := b.Jitter()
		if lat > 0 && (minLatency < 0 || lat < minLatency) {
			minLatency = lat
		}
		if jit > 0 && (minJitter < 0 || jit < minJitter) {
			minJitter = jit
		}
	}

	// If no data, use defaults
	if minLatency <= 0 {
		minLatency = 1
	}
	if minJitter <= 0 {
		minJitter = 1
	}

	var best *Backend
	bestScore := -1

	for _, b := range available {
		score := b.RelativeHealthScore(minLatency, minJitter)

		// Add historical score adjustment
		if history != nil {
			histScore := history.HistoricalScore(b.Addr, b.AvgLatency(), b.Jitter())
			score += histScore
		}

		if score > bestScore {
			bestScore = score
			best = b
		}
	}
	return best
}

type SequentialStrategy struct{}

func (s *SequentialStrategy) Name() string {
	return "sequential"
}

func (s *SequentialStrategy) Select(backends []*Backend, jitterThreshold float64, history *HistoryManager) *Backend {
	for _, b := range backends {
		if b.IsAvailable() {
			return b
		}
	}
	return nil
}

type RandomStrategy struct{}

func (s *RandomStrategy) Name() string {
	return "random"
}

func (s *RandomStrategy) Select(backends []*Backend, jitterThreshold float64, history *HistoryManager) *Backend {
	available := filterAvailable(backends)
	if len(available) == 0 {
		return nil
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(available))))
	if err != nil {
		return available[0]
	}
	return available[n.Int64()]
}

func filterAvailable(backends []*Backend) []*Backend {
	var result []*Backend
	for _, b := range backends {
		if b.IsAvailable() {
			result = append(result, b)
		}
	}
	return result
}

func GetStrategy(name string) Strategy {
	switch name {
	case "round-robin":
		return &RoundRobinStrategy{}
	case "least-connections":
		return &LeastConnectionsStrategy{}
	case "health-score":
		return &HealthScoreStrategy{}
	case "sequential":
		return &SequentialStrategy{}
	case "random":
		return &RandomStrategy{}
	default:
		return &HealthScoreStrategy{}
	}
}
