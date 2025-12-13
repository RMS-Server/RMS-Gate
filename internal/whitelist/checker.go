package whitelist

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

type CheckResult int

const (
	Allowed CheckResult = iota
	NotInWhitelist
	ServerError
)

type Checker struct {
	client *http.Client
	log    logr.Logger
}

func NewChecker(log logr.Logger) *Checker {
	return &Checker{
		client: &http.Client{},
		log:    log,
	}
}

type whitelistRequest struct {
	Username string `json:"username"`
	UUID     string `json:"uuid"`
}

func (w *Checker) Check(ctx context.Context, username, uuid, baseURL string, timeoutSeconds int) CheckResult {
	reqBody := whitelistRequest{
		Username: username,
		UUID:     uuid,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		w.log.Error(err, "Failed to marshal request")
		return ServerError
	}

	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	apiURL := strings.TrimSuffix(baseURL, "/") + "/api/whitelist"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		w.log.Error(err, "Failed to create request")
		return ServerError
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		w.log.Error(err, "Whitelist check failed", "username", username, "uuid", uuid)
		return ServerError
	}
	defer resp.Body.Close()

	w.log.Info("Whitelist check response", "username", username, "uuid", uuid, "status", resp.StatusCode)

	switch resp.StatusCode {
	case http.StatusOK:
		return Allowed
	case http.StatusForbidden:
		return NotInWhitelist
	default:
		return ServerError
	}
}
