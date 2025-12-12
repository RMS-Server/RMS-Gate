package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	emaAlpha         = 0.1 // EMA smoothing factor: 10% new data, 90% history
	minSamplesForUse = 20  // Minimum samples before using historical data
)

// PeriodStats stores statistics for a specific 15-minute period
type PeriodStats struct {
	AvgLatency  float64 `json:"avgLatency"`
	AvgJitter   float64 `json:"avgJitter"`
	Samples     int     `json:"samples"`
	PeriodIndex int     `json:"periodIndex"` // 0-95, stable slot within local day
	PeriodLabel string  `json:"periodLabel"` // "HH:MM-HH:MM" in local timezone
}

// BackendHistory stores statistics for 96 periods (24 hours * 4 periods per hour)
type BackendHistory struct {
	PeriodStats [96]*PeriodStats `json:"periodStats"` // Index 0-95 for 15-min periods
}

// HistoryManager manages historical statistics for all backends
type HistoryManager struct {
	mu       sync.RWMutex
	backends map[string]*BackendHistory // key: backend address
	filePath string
	dirty    bool
}

func NewHistoryManager(dataDir string) *HistoryManager {
	hm := &HistoryManager{
		backends: make(map[string]*BackendHistory),
		filePath: filepath.Join(dataDir, "lb_history.json"),
	}
	hm.load()
	return hm
}

// getPeriodIndex returns the current 15-minute period index (0-95)
func getPeriodIndex() int {
	now := time.Now()
	return now.Hour()*4 + now.Minute()/15
}

func periodLabel(period int) string {
	if period < 0 || period > 95 {
		return ""
	}
	startMin := period * 15
	endMin := (startMin + 15) % (24 * 60)
	sh, sm := startMin/60, startMin%60
	eh, em := endMin/60, endMin%60
	return fmt.Sprintf("%02d:%02d-%02d:%02d", sh, sm, eh, em)
}

// Record records a new sample for a backend at the current 15-minute period
func (hm *HistoryManager) Record(addr string, latency, jitter float64) {
	period := getPeriodIndex()

	hm.mu.Lock()
	defer hm.mu.Unlock()

	history, ok := hm.backends[addr]
	if !ok {
		history = &BackendHistory{}
		for i := range history.PeriodStats {
			history.PeriodStats[i] = &PeriodStats{}
		}
		hm.backends[addr] = history
	}

	stats := history.PeriodStats[period]
	// Backfill period metadata for export/debugging.
	if stats.PeriodLabel == "" {
		stats.PeriodIndex = period
		stats.PeriodLabel = periodLabel(period)
	}
	if stats.Samples == 0 {
		// First sample for this period
		stats.AvgLatency = latency
		stats.AvgJitter = jitter
	} else {
		// EMA update
		stats.AvgLatency = emaAlpha*latency + (1-emaAlpha)*stats.AvgLatency
		stats.AvgJitter = emaAlpha*jitter + (1-emaAlpha)*stats.AvgJitter
	}
	stats.Samples++
	hm.dirty = true
}

// GetPeriodStats returns statistics for a backend at a specific 15-minute period
func (hm *HistoryManager) GetPeriodStats(addr string, period int) *PeriodStats {
	if period < 0 || period > 95 {
		return nil
	}

	hm.mu.RLock()
	defer hm.mu.RUnlock()

	history, ok := hm.backends[addr]
	if !ok {
		return nil
	}
	return history.PeriodStats[period]
}

// GetCurrentPeriodStats returns statistics for a backend at the current 15-minute period
func (hm *HistoryManager) GetCurrentPeriodStats(addr string) *PeriodStats {
	return hm.GetPeriodStats(addr, getPeriodIndex())
}

// HistoricalScore returns a score adjustment based on historical data
// Positive = performing better than historical average
// Negative = performing worse than historical average
func (hm *HistoryManager) HistoricalScore(addr string, currentLatency, currentJitter float64) int {
	stats := hm.GetCurrentPeriodStats(addr)
	if stats == nil || stats.Samples < minSamplesForUse {
		return 0 // Not enough data
	}

	var score float64 = 0

	// Latency comparison (max ±8 points)
	if stats.AvgLatency > 0 && currentLatency > 0 {
		latencyRatio := currentLatency / stats.AvgLatency
		if latencyRatio < 0.7 {
			// 30%+ better than history
			score += 8
		} else if latencyRatio < 0.85 {
			// 15-30% better
			score += 4
		} else if latencyRatio > 1.5 {
			// 50%+ worse than history
			score -= 8
		} else if latencyRatio > 1.2 {
			// 20-50% worse
			score -= 4
		}
	}

	// Jitter comparison (max ±4 points)
	if stats.AvgJitter > 0 && currentJitter > 0 {
		jitterRatio := currentJitter / stats.AvgJitter
		if jitterRatio < 0.7 {
			score += 4
		} else if jitterRatio > 1.5 {
			score -= 4
		}
	}

	return int(score)
}

// Save persists the history to disk
func (hm *HistoryManager) Save() error {
	hm.mu.RLock()
	if !hm.dirty {
		hm.mu.RUnlock()
		return nil
	}
	data, err := json.MarshalIndent(hm.backends, "", "  ")
	hm.mu.RUnlock()

	if err != nil {
		return err
	}

	dir := filepath.Dir(hm.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	if err := os.WriteFile(hm.filePath, data, 0644); err != nil {
		return err
	}

	hm.mu.Lock()
	hm.dirty = false
	hm.mu.Unlock()

	return nil
}

func (hm *HistoryManager) load() {
	data, err := os.ReadFile(hm.filePath)
	if err != nil {
		return // File doesn't exist, start fresh
	}

	var backends map[string]*BackendHistory
	if err := json.Unmarshal(data, &backends); err != nil {
		return
	}

	// Initialize nil period stats
	for _, history := range backends {
		for i := range history.PeriodStats {
			if history.PeriodStats[i] == nil {
				history.PeriodStats[i] = &PeriodStats{}
			}
			// Backfill metadata for older history files.
			if history.PeriodStats[i].PeriodLabel == "" {
				history.PeriodStats[i].PeriodIndex = i
				history.PeriodStats[i].PeriodLabel = periodLabel(i)
			}
		}
	}

	hm.backends = backends
}

// StartAutoSave starts a goroutine that periodically saves history
func (hm *HistoryManager) StartAutoSave(interval time.Duration, stopCh <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				hm.Save()
			case <-stopCh:
				hm.Save() // Save on shutdown
				return
			}
		}
	}()
}

// GetAllStats returns all historical data (for debugging/display)
func (hm *HistoryManager) GetAllStats() map[string]*BackendHistory {
	hm.mu.RLock()
	defer hm.mu.RUnlock()

	result := make(map[string]*BackendHistory, len(hm.backends))
	for k, v := range hm.backends {
		result[k] = v
	}
	return result
}
