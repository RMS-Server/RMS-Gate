package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

const (
	PermissionLevelAdmin = 3
)

type PermissionManager struct {
	client       *http.Client
	log          logr.Logger
	baseURL      string
	cache        map[string]int // username -> permission_level
	cacheMu      sync.RWMutex
	cacheExpiry  time.Time
	cacheTTL     time.Duration
	adminCommands []string
}

type permissionResponse struct {
	Success bool `json:"success"`
	Users   []struct {
		Username        string `json:"username"`
		PermissionLevel int    `json:"permission_level"`
	} `json:"users"`
}

func NewPermissionManager(log logr.Logger, baseURL string, cacheTTLSeconds int, adminCommands []string) *PermissionManager {
	return &PermissionManager{
		client:        &http.Client{Timeout: 10 * time.Second},
		log:           log.WithName("permission"),
		baseURL:       strings.TrimSuffix(baseURL, "/"),
		cache:         make(map[string]int),
		cacheTTL:      time.Duration(cacheTTLSeconds) * time.Second,
		adminCommands: adminCommands,
	}
}

func (p *PermissionManager) fetchPermissions(ctx context.Context) error {
	url := p.baseURL + "/api/mcdr/permission"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result permissionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	if !result.Success {
		p.log.Info("Permission API returned success=false")
		return nil
	}

	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	p.cache = make(map[string]int)
	for _, user := range result.Users {
		p.cache[strings.ToLower(user.Username)] = user.PermissionLevel
	}
	p.cacheExpiry = time.Now().Add(p.cacheTTL)

	p.log.Info("Permission cache refreshed", "users", len(result.Users))
	return nil
}

func (p *PermissionManager) GetPermissionLevel(ctx context.Context, username string) int {
	p.cacheMu.RLock()
	expired := time.Now().After(p.cacheExpiry)
	level, exists := p.cache[strings.ToLower(username)]
	p.cacheMu.RUnlock()

	if expired || !exists {
		if err := p.fetchPermissions(ctx); err != nil {
			p.log.Error(err, "Failed to fetch permissions")
			if exists {
				return level
			}
			return 0
		}

		p.cacheMu.RLock()
		level = p.cache[strings.ToLower(username)]
		p.cacheMu.RUnlock()
	}

	return level
}

func (p *PermissionManager) IsAdmin(ctx context.Context, username string) bool {
	return p.GetPermissionLevel(ctx, username) > PermissionLevelAdmin
}

func (p *PermissionManager) IsAdminCommand(cmd string) bool {
	cmdLower := strings.ToLower(strings.TrimPrefix(cmd, "/"))
	parts := strings.Fields(cmdLower)
	if len(parts) == 0 {
		return false
	}
	cmdName := parts[0]

	for _, adminCmd := range p.adminCommands {
		if strings.ToLower(adminCmd) == cmdName {
			return true
		}
	}
	return false
}

func (p *PermissionManager) CanExecute(ctx context.Context, username, cmd string) bool {
	if !p.IsAdminCommand(cmd) {
		return true
	}
	return p.IsAdmin(ctx, username)
}
