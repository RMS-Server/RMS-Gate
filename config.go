package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
)

type Config struct {
	APIUrl            string               `json:"apiUrl"`
	TimeoutSeconds    int                  `json:"timeoutSeconds"`
	MsgNotInWhitelist string               `json:"msgNotInWhitelist"`
	MsgServerError    string               `json:"msgServerError"`
	MCSManager        *MCSManagerConfig    `json:"mcsManager"`
	DynamicServer     *DynamicServerConfig `json:"dynamicServer"`
	Permission        *PermissionConfig    `json:"permission"`
}

type PermissionConfig struct {
	Enabled             bool     `json:"enabled"`
	CacheTTLSeconds     int      `json:"cacheTtlSeconds"`
	AdminCommands       []string `json:"adminCommands"`
	MsgNoPermission     string   `json:"msgNoPermission"`
}

type MCSManagerConfig struct {
	BaseURL  string `json:"baseUrl"`
	APIKey   string `json:"apiKey"`
	DaemonID string `json:"daemonId"`
}

type DynamicServerConfig struct {
	ServerUUIDMap              map[string]string `json:"serverUuidMap"`
	AutoStartServers           []string          `json:"autoStartServers"`
	StartupTimeoutSeconds      int               `json:"startupTimeoutSeconds"`
	PollIntervalSeconds        int               `json:"pollIntervalSeconds"`
	ConnectivityTimeoutSeconds int               `json:"connectivityTimeoutSeconds"`
	IdleShutdownSeconds        int               `json:"idleShutdownSeconds"`
	MsgStarting                string            `json:"msgStarting"`
	MsgStartupTimeout          string            `json:"msgStartupTimeout"`
}

func defaultConfig() *Config {
	return &Config{
		APIUrl:            "http://localhost:8080/api/whitelist",
		TimeoutSeconds:    10,
		MsgNotInWhitelist: "您当前不在白名单中",
		MsgServerError:    "500服务器内部错误，请联系管理员",
		MCSManager: &MCSManagerConfig{
			BaseURL:  "https://mcsm.example.com/api",
			APIKey:   "your-api-key",
			DaemonID: "your-daemon-id",
		},
		DynamicServer: &DynamicServerConfig{
			ServerUUIDMap:              map[string]string{},
			AutoStartServers:           []string{},
			StartupTimeoutSeconds:      60,
			PollIntervalSeconds:        2,
			ConnectivityTimeoutSeconds: 30,
			IdleShutdownSeconds:        60,
			MsgStarting:                "正在启动服务器 %s，请稍候...",
			MsgStartupTimeout:          "服务器 %s 启动超时，请稍后重试",
		},
		Permission: &PermissionConfig{
			Enabled:         true,
			CacheTTLSeconds: 300,
			AdminCommands:   []string{"send", "dserver", "glist", "server"},
			MsgNoPermission: "你没有权限执行此命令",
		},
	}
}

func loadConfig(configDir string, log logr.Logger) *Config {
	configPath := filepath.Join(configDir, "config.json")

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.MkdirAll(configDir, 0755); err != nil {
			log.Error(err, "Failed to create config directory")
			return defaultConfig()
		}
		cfg := defaultConfig()
		if err := saveConfig(configPath, cfg); err != nil {
			log.Error(err, "Failed to save default config")
		} else {
			log.Info("Default configuration created", "path", configPath)
		}
		return cfg
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Error(err, "Failed to read config file")
		return defaultConfig()
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Error(err, "Failed to parse config file")
		return defaultConfig()
	}

	log.Info("Configuration loaded successfully")
	return &cfg
}

func saveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
