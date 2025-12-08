package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

type MCSManagerClient struct {
	log      logr.Logger
	client   *http.Client
	baseURL  string
	apiKey   string
	daemonID string
}

func NewMCSManagerClient(log logr.Logger, cfg *MCSManagerConfig) *MCSManagerClient {
	return &MCSManagerClient{
		log:      log.WithName("mcsmanager"),
		client:   &http.Client{Timeout: 30 * time.Second},
		baseURL:  cfg.BaseURL,
		apiKey:   cfg.APIKey,
		daemonID: cfg.DaemonID,
	}
}

type instanceListResponse struct {
	Status int `json:"status"`
	Data   struct {
		Data []struct {
			InstanceUUID string `json:"instanceUuid"`
			Status       int    `json:"status"`
		} `json:"data"`
	} `json:"data"`
}

type apiResponse struct {
	Status int    `json:"status"`
	Data   any    `json:"data"`
	Error  string `json:"err"`
}

func (m *MCSManagerClient) StartInstance(ctx context.Context, instanceUUID string) (bool, error) {
	url := fmt.Sprintf("%s/protected_instance/open?uuid=%s&daemonId=%s&apikey=%s",
		m.baseURL, instanceUUID, m.daemonID, m.apiKey)

	m.log.V(1).Info("Starting instance", "uuid", instanceUUID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		m.log.Error(nil, "Failed to start instance", "uuid", instanceUUID, "status", resp.StatusCode)
		return false, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	var result apiResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return false, err
	}

	if result.Error != "" {
		m.log.Error(nil, "API error starting instance", "uuid", instanceUUID, "error", result.Error)
		return false, nil
	}

	m.log.Info("Successfully sent start command", "uuid", instanceUUID)
	return true, nil
}

func (m *MCSManagerClient) StopInstance(ctx context.Context, instanceUUID string) (bool, error) {
	url := fmt.Sprintf("%s/protected_instance/stop?uuid=%s&daemonId=%s&apikey=%s",
		m.baseURL, instanceUUID, m.daemonID, m.apiKey)

	m.log.V(1).Info("Stopping instance", "uuid", instanceUUID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		m.log.Error(nil, "Failed to stop instance", "uuid", instanceUUID, "status", resp.StatusCode)
		return false, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	var result apiResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return false, err
	}

	if result.Error != "" {
		m.log.Error(nil, "API error stopping instance", "uuid", instanceUUID, "error", result.Error)
		return false, nil
	}

	m.log.Info("Successfully sent stop command", "uuid", instanceUUID)
	return true, nil
}

// GetInstanceStatus returns instance status:
// 0: stopped, 1: stopping, 2: starting, 3: running
func (m *MCSManagerClient) GetInstanceStatus(ctx context.Context, instanceUUID string) (int, error) {
	url := fmt.Sprintf("%s/service/remote_service_instances?daemonId=%s&page=1&page_size=100&apikey=%s",
		m.baseURL, m.daemonID, m.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 2, err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return 2, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		m.log.Error(nil, "Failed to get instance status", "uuid", instanceUUID, "status", resp.StatusCode)
		return 2, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 2, err
	}

	var result instanceListResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return 2, err
	}

	for _, inst := range result.Data.Data {
		if inst.InstanceUUID == instanceUUID {
			m.log.V(1).Info("Instance status", "uuid", instanceUUID, "status", inst.Status)
			return inst.Status, nil
		}
	}

	m.log.Info("Instance not found in list", "uuid", instanceUUID)
	return 2, nil
}

func (m *MCSManagerClient) IsInstanceRunning(ctx context.Context, instanceUUID string) (bool, error) {
	status, err := m.GetInstanceStatus(ctx, instanceUUID)
	if err != nil {
		return false, err
	}
	return status == 3, nil
}
