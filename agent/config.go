// Package agent embeds the node agent directly into the sing-box process.
// It replaces the old two-process setup (external agent + clash API over
// HTTP) with in-process control of the box instance.
package agent

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// NodeConfig is the node-local configuration file (node.yaml).
type NodeConfig struct {
	NodeCode      string                 `yaml:"node_code"`
	NodeSecret    string                 `yaml:"node_secret"`
	APIURL        string                 `yaml:"api_url"`
	CoreType      string                 `yaml:"core_type"`
	Name          string                 `yaml:"name"`
	Region        string                 `yaml:"region"`
	Host          string                 `yaml:"host"`
	Port          int                    `yaml:"port"`
	BandwidthMbps int                    `yaml:"bandwidth_mbps"`
	MaxUsers      int                    `yaml:"max_users"`
	Weight        int                    `yaml:"weight"`
	Protocols     map[string]interface{} `yaml:"protocols"`
}

// LoadConfig reads and parses the node config file.
func LoadConfig(path string) (*NodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg NodeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}
