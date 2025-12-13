package loadbalancer

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	emaAlpha         = 0.1 // EMA smoothing factor: 10% new data, 90% history
	minSamplesForUse = 20  // Minimum samples before using historical data
)

// PeriodStats stores statistics for a specific 15-minute period
type PeriodStats struct {
	AvgLatency  float64
	AvgJitter   float64
	Samples     int
	PeriodIndex int    // 0-95, stable slot within local day
	PeriodLabel string // "HH:MM-HH:MM" in local timezone
}

// BackendHistory stores statistics for 96 periods (24 hours * 4 periods per hour)
type BackendHistory struct {
	PeriodStats [96]*PeriodStats
}

// HistoryManager manages historical statistics for all backends
type HistoryManager struct {
	mu     sync.RWMutex
	db     *sql.DB
	dbPath string

	// In-memory cache for fast reads
	cache map[string]*BackendHistory
}

func NewHistoryManager(dataDir string) *HistoryManager {
	dbPath := filepath.Join(dataDir, "lb_history.db")
	hm := &HistoryManager{
		dbPath: dbPath,
		cache:  make(map[string]*BackendHistory),
	}
	hm.initDB()
	hm.loadFromDB()
	return hm
}

func (hm *HistoryManager) initDB() {
	db, err := sql.Open("sqlite3", hm.dbPath)
	if err != nil {
		return
	}
	hm.db = db

	// Create table if not exists
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS period_stats (
			backend_addr TEXT NOT NULL,
			period_index INTEGER NOT NULL,
			period_label TEXT NOT NULL,
			avg_latency REAL NOT NULL,
			avg_jitter REAL NOT NULL,
			samples INTEGER NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (backend_addr, period_index)
		)
	`)

	// Create index for faster lookups
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_backend_addr ON period_stats(backend_addr)`)
}

func (hm *HistoryManager) loadFromDB() {
	if hm.db == nil {
		return
	}

	rows, err := hm.db.Query(`
		SELECT backend_addr, period_index, period_label, avg_latency, avg_jitter, samples
		FROM period_stats
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	hm.mu.Lock()
	defer hm.mu.Unlock()

	for rows.Next() {
		var addr string
		var periodIndex int
		var periodLabel string
		var avgLatency, avgJitter float64
		var samples int

		if err := rows.Scan(&addr, &periodIndex, &periodLabel, &avgLatency, &avgJitter, &samples); err != nil {
			continue
		}

		history, ok := hm.cache[addr]
		if !ok {
			history = &BackendHistory{}
			for i := range history.PeriodStats {
				history.PeriodStats[i] = &PeriodStats{
					PeriodIndex: i,
					PeriodLabel: periodLabelFromIndex(i),
				}
			}
			hm.cache[addr] = history
		}

		if periodIndex >= 0 && periodIndex < 96 {
			history.PeriodStats[periodIndex] = &PeriodStats{
				AvgLatency:  avgLatency,
				AvgJitter:   avgJitter,
				Samples:     samples,
				PeriodIndex: periodIndex,
				PeriodLabel: periodLabel,
			}
		}
	}
}

// getPeriodIndex returns the current 15-minute period index (0-95)
func getPeriodIndex() int {
	now := time.Now()
	return now.Hour()*4 + now.Minute()/15
}

func periodLabelFromIndex(period int) string {
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

	history, ok := hm.cache[addr]
	if !ok {
		history = &BackendHistory{}
		for i := range history.PeriodStats {
			history.PeriodStats[i] = &PeriodStats{
				PeriodIndex: i,
				PeriodLabel: periodLabelFromIndex(i),
			}
		}
		hm.cache[addr] = history
	}

	stats := history.PeriodStats[period]
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

	// Write to DB asynchronously
	go hm.savePeriodStats(addr, stats)
}

func (hm *HistoryManager) savePeriodStats(addr string, stats *PeriodStats) {
	if hm.db == nil {
		return
	}

	_, _ = hm.db.Exec(`
		INSERT OR REPLACE INTO period_stats
		(backend_addr, period_index, period_label, avg_latency, avg_jitter, samples, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, addr, stats.PeriodIndex, stats.PeriodLabel, stats.AvgLatency, stats.AvgJitter, stats.Samples)
}

// GetPeriodStats returns statistics for a backend at a specific 15-minute period
func (hm *HistoryManager) GetPeriodStats(addr string, period int) *PeriodStats {
	if period < 0 || period > 95 {
		return nil
	}

	hm.mu.RLock()
	defer hm.mu.RUnlock()

	history, ok := hm.cache[addr]
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

// Save is kept for compatibility but now does nothing (writes are immediate)
func (hm *HistoryManager) Save() error {
	return nil
}

// StartAutoSave is kept for compatibility but now does nothing
func (hm *HistoryManager) StartAutoSave(interval time.Duration, stopCh <-chan struct{}) {
	go func() {
		<-stopCh
		hm.Close()
	}()
}

// Close closes the database connection
func (hm *HistoryManager) Close() error {
	if hm.db != nil {
		return hm.db.Close()
	}
	return nil
}

// GetAllStats returns all historical data (for debugging/display)
func (hm *HistoryManager) GetAllStats() map[string]*BackendHistory {
	hm.mu.RLock()
	defer hm.mu.RUnlock()

	result := make(map[string]*BackendHistory, len(hm.cache))
	for k, v := range hm.cache {
		result[k] = v
	}
	return result
}
