package api

// NEW: node agent API types, kept in sync with backend/cmd/agent.

// HeartbeatData is the payload posted to /internal/node/heartbeat.
type HeartbeatData struct {
	NodeCode    string  `json:"node_code"`
	CPU         float64 `json:"cpu"`
	Memory      float64 `json:"memory"`
	OnlineUsers int     `json:"online_users"`
	TrafficIn   int64   `json:"traffic_in"`
	TrafficOut  int64   `json:"traffic_out"`
}

// ServerCommand is the response of the heartbeat endpoint.
type ServerCommand struct {
	Reload  bool `json:"reload"`
	Upgrade bool `json:"upgrade"`
}

// BlockedUser is a blacklisted user entry.
type BlockedUser struct {
	UUID     string `json:"uuid"`
	Reason   string `json:"reason"`
	ExpireAt int64  `json:"expire_at"`
}

// BlockedDevice is a user+IP combination blocked on the panel (device
// removed by the user). Nodes must reject reconnects from that IP.
type BlockedDevice struct {
	UUID string `json:"uuid"`
	IP   string `json:"ip"`
}

// BlacklistResponse is the body of GET /internal/nodes/blacklist.
type BlacklistResponse struct {
	Blocked        []BlockedUser   `json:"blocked"`
	BlockedDevices []BlockedDevice `json:"blocked_devices"`
}

// NodeUser is a user entry of the node config response.
type NodeUser struct {
	UserID         int64  `json:"user_id"`
	UUID           string `json:"uuid"`
	Password       string `json:"password"`
	SpeedLimit     int    `json:"speed_limit"`     // mbps, 0 = unlimited
	MaxConnections int    `json:"max_connections"` // 0 = unlimited
	DeviceLimit    int    `json:"device_limit"`    // 0 = unlimited
}

// NodeConfigResponse is the data field of /internal/node/config.
type NodeConfigResponse struct {
	NodeID    int64                  `json:"node_id"`
	NodeCode  string                 `json:"node_code"`
	Host      string                 `json:"host"`
	Port      int                    `json:"port"`
	CoreType  string                 `json:"core_type"`
	Protocols map[string]interface{} `json:"protocols"`
	Users     []NodeUser             `json:"users"`
}

// TrafficRecord is a single per-user traffic delta report entry.
type TrafficRecord struct {
	UserID   int64 `json:"user_id"`
	NodeID   int64 `json:"node_id"`
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
}

// DeviceRecord is a single per-user online device report entry: the source
// IPs that currently hold at least one connection for the user.
type DeviceRecord struct {
	UUID string   `json:"uuid"`
	IPs  []string `json:"ips"`
}

// DeviceReport is the payload posted to /internal/node/devices.
type DeviceReport struct {
	NodeID  int64          `json:"node_id"`
	Devices []DeviceRecord `json:"devices"`
}
