// Package api implements all HTTP interactions between the node agent and
// the central panel API. The agent itself never talks HTTP directly.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is the central panel API client for one node.
type Client struct {
	baseURL    string
	nodeCode   string
	nodeSecret string
	httpClient *http.Client
}

// NewClient creates a panel API client.
func NewClient(baseURL, nodeCode, nodeSecret string) *Client {
	return &Client{
		baseURL:    baseURL,
		nodeCode:   nodeCode,
		nodeSecret: nodeSecret,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Register posts the node registration payload to /internal/node/register.
func (c *Client) Register(payload map[string]interface{}) error {
	return c.postJSON("/internal/node/register", payload, nil)
}

// Heartbeat posts the node status and returns the server command.
func (c *Client) Heartbeat(data HeartbeatData) (*ServerCommand, error) {
	var cmd ServerCommand
	if err := c.postJSON("/internal/node/heartbeat", data, &cmd); err != nil {
		return nil, err
	}
	return &cmd, nil
}

// Blacklist fetches the blocked user/device list from
// /internal/nodes/blacklist. The response is a plain JSON body, not a
// wrapped response.
func (c *Client) Blacklist() (*BlacklistResponse, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/internal/nodes/blacklist", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Node-Code", c.nodeCode)
	req.Header.Set("X-Node-Secret", c.nodeSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("blacklist sync failed: %s", string(body))
	}

	var result BlacklistResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// NodeConfig fetches the node config from /internal/node/config and unwraps
// the {"code": ..., "data": {...}} envelope.
func (c *Client) NodeConfig() (*NodeConfigResponse, error) {
	req, err := http.NewRequest("GET", c.baseURL+"/internal/node/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Node-Code", c.nodeCode)
	req.Header.Set("X-Node-Secret", c.nodeSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("config fetch failed: %s", string(body))
	}

	var result struct {
		Data NodeConfigResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// ReportTraffic posts per-user traffic delta records to /internal/traffic/report.
// The body is a plain JSON array.
func (c *Client) ReportTraffic(records []TrafficRecord) error {
	return c.postJSON("/internal/traffic/report", records, nil)
}

// ReportDevices posts per-user online device (source IP) records to
// /internal/node/devices.
func (c *Client) ReportDevices(report DeviceReport) error {
	return c.postJSON("/internal/node/devices", report, nil)
}

func (c *Client) postJSON(path string, payload, response interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", c.baseURL+path, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-Secret", c.nodeSecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if response != nil {
		return json.NewDecoder(resp.Body).Decode(response)
	}
	return nil
}
