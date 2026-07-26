package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/agent/api"
	"github.com/sagernet/sing-box/common/ratelimiter"
	"github.com/sagernet/sing-box/common/trafficcontrol"
	"github.com/sagernet/sing-box/experimental/deprecated"
	"github.com/sagernet/sing-box/include"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/service"
)

// Agent runs the node agent inside the sing-box process, controlling the box
// instance directly instead of through the clash API over HTTP.
type Agent struct {
	cfg         *NodeConfig
	apiClient   *api.Client
	realityKeys *realityKeyPair

	inboundManager adapter.InboundManager
	trafficManager *trafficcontrol.Manager
	rateLimiter    *ratelimiter.Manager

	blockListMu    sync.RWMutex
	blockedUUIDs   map[string]bool
	blockedDevices map[string]bool // "uuid|ip" pairs blocked on the panel

	usersMu     sync.RWMutex
	cachedUsers []api.NodeUser          // active (non-blocked) users, last applied
	nodeConfig  *api.NodeConfigResponse // last fetched node config, for uuid -> user_id mapping

	trafficMu   sync.Mutex
	lastTraffic map[string][2]int64

	done chan struct{}
	wg   sync.WaitGroup
}

// Run loads the node config, starts sing-box in-process and runs the agent
// loops until SIGINT/SIGTERM.
func Run(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.Host == "" || cfg.Port == 0 {
		return fmt.Errorf("%s missing required fields: host, port", configPath)
	}

	// VLESS+Reality: load or generate the keypair locally, and inject the
	// public parameters into the protocols config so the panel can build
	// client subscription links. The private key never leaves the node.
	var realityKeys *realityKeyPair
	if isRealityEnabled(cfg.Protocols) {
		keys, err := ensureRealityKeys("certs/reality.json", cfg.Protocols)
		if err != nil {
			return fmt.Errorf("prepare reality keys: %w", err)
		}
		realityKeys = keys
		cfg.Protocols["security"] = "reality"
		cfg.Protocols["reality_public_key"] = keys.PublicKey
		cfg.Protocols["reality_short_id"] = keys.ShortID
		log.Printf("Reality enabled, public key: %s", keys.PublicKey)
	}

	apiClient := api.NewClient(cfg.APIURL, cfg.NodeCode, cfg.NodeSecret)

	// Register node
	payload := map[string]interface{}{
		"node_code":      cfg.NodeCode,
		"node_secret":    cfg.NodeSecret,
		"core_type":      cfg.CoreType,
		"name":           cfg.Name,
		"region":         cfg.Region,
		"host":           cfg.Host,
		"port":           cfg.Port,
		"bandwidth_mbps": cfg.BandwidthMbps,
		"max_users":      cfg.MaxUsers,
		"weight":         cfg.Weight,
		"protocols":      cfg.Protocols,
	}
	if err := apiClient.Register(payload); err != nil {
		return fmt.Errorf("register node: %w", err)
	}

	// The generated sing-box config logs to logs/sing-box.log.
	if err := os.MkdirAll("logs", 0755); err != nil {
		return fmt.Errorf("create logs directory: %w", err)
	}

	// Fetch the initial node config and build sing-box options from it.
	nc, err := apiClient.NodeConfig()
	if err != nil {
		return fmt.Errorf("fetch node config: %w", err)
	}
	activeUsers := filterBlocked(nc.Users, nil)

	// Service context with registries, same as cmd_run.go's create().
	ctx := service.ContextWith(context.Background(), deprecated.NewStderrManager(sblog.StdLogger()))
	ctx = include.Context(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	options, err := buildOptions(ctx, nc, activeUsers, realityKeys)
	if err != nil {
		return fmt.Errorf("build sing-box options: %w", err)
	}
	instance, err := box.New(box.Options{
		Context: ctx,
		Options: options,
	})
	if err != nil {
		return fmt.Errorf("create sing-box: %w", err)
	}
	if err := instance.Start(); err != nil {
		cancel()
		return fmt.Errorf("start sing-box: %w", err)
	}

	a := &Agent{
		cfg:            cfg,
		apiClient:      apiClient,
		realityKeys:    realityKeys,
		blockedUUIDs:   make(map[string]bool),
		blockedDevices: make(map[string]bool),
		lastTraffic:    make(map[string][2]int64),
		done:           make(chan struct{}),
		inboundManager: service.FromContext[adapter.InboundManager](ctx),
		trafficManager: service.PtrFromContext[trafficcontrol.Manager](ctx),
		rateLimiter:    service.FromContext[*ratelimiter.Manager](ctx),
	}
	if a.inboundManager == nil {
		cancel()
		return fmt.Errorf("inbound manager not found in service context")
	}
	if a.trafficManager == nil {
		cancel()
		return fmt.Errorf("traffic manager not found in service context")
	}
	if a.rateLimiter == nil {
		// Should not happen: the clash_api block (with empty
		// external_controller) makes box.New create and register it.
		cancel()
		return fmt.Errorf("rate limiter not found in service context")
	}

	// Apply initial per-user limits and remember the user list.
	a.applyUserConfig(nc, activeUsers)

	log.Printf("Node agent started: %s", cfg.NodeCode)

	a.wg.Add(5)
	go a.loop(30*time.Second, a.sendHeartbeat)
	go a.loop(30*time.Second, a.syncBlacklist)
	go a.loop(60*time.Second, a.syncConfig)
	go a.loop(60*time.Second, a.reportTraffic)
	go a.loop(60*time.Second, a.reportDevices)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()

	log.Println("Shutting down...")
	close(a.done)
	a.wg.Wait()
	cancel()
	return instance.Close()
}

// loop runs fn on every tick until the agent is shut down.
func (a *Agent) loop(interval time.Duration, fn func() error) {
	defer a.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			if err := fn(); err != nil {
				log.Printf("%v", err)
			}
		}
	}
}

// filterBlocked returns the users not present in the blocked set.
func filterBlocked(users []api.NodeUser, blocked map[string]bool) []api.NodeUser {
	active := make([]api.NodeUser, 0, len(users))
	for _, u := range users {
		if !blocked[u.UUID] {
			active = append(active, u)
		} else {
			log.Printf("Filtered blocked user: %s", u.UUID)
		}
	}
	return active
}

func (a *Agent) getBlocked() map[string]bool {
	a.blockListMu.RLock()
	defer a.blockListMu.RUnlock()
	blocked := make(map[string]bool, len(a.blockedUUIDs))
	for k, v := range a.blockedUUIDs {
		blocked[k] = v
	}
	return blocked
}

// sendHeartbeat posts node status to the panel and handles server commands.
func (a *Agent) sendHeartbeat() error {
	var trafficIn, trafficOut int64
	for _, t := range a.trafficManager.UserTraffic() {
		trafficIn += t[0]
		trafficOut += t[1]
	}
	cmd, err := a.apiClient.Heartbeat(api.HeartbeatData{
		NodeCode:    a.cfg.NodeCode,
		CPU:         getCPUUsage(),
		Memory:      getMemoryUsage(),
		OnlineUsers: a.trafficManager.OnlineUsers(),
		TrafficIn:   trafficIn,
		TrafficOut:  trafficOut,
	})
	if err != nil {
		return fmt.Errorf("heartbeat failed: %w", err)
	}
	if cmd.Reload {
		log.Println("Server requested reload")
		if err := a.reloadConfig(); err != nil {
			log.Printf("Reload failed: %v", err)
		}
	}
	if cmd.Upgrade {
		log.Println("Server requested upgrade")
	}
	return nil
}

// reloadConfig re-fetches the node config and hot-applies it
// (user list + per-user limits) without restarting sing-box.
func (a *Agent) reloadConfig() error {
	nc, err := a.apiClient.NodeConfig()
	if err != nil {
		return err
	}
	activeUsers := filterBlocked(nc.Users, a.getBlocked())
	a.applyUserConfig(nc, activeUsers)
	log.Println("Node config reloaded and applied")
	return nil
}

// syncConfig polls the node config every minute and applies changes to the
// user list, speed limits and connection/device limits.
func (a *Agent) syncConfig() error {
	nc, err := a.apiClient.NodeConfig()
	if err != nil {
		return fmt.Errorf("config sync failed: %w", err)
	}
	activeUsers := filterBlocked(nc.Users, a.getBlocked())

	a.usersMu.RLock()
	changed := usersChanged(a.cachedUsers, activeUsers)
	a.usersMu.RUnlock()

	if !changed {
		// Keep the uuid -> user_id mapping fresh for traffic reports.
		a.usersMu.Lock()
		a.nodeConfig = nc
		a.usersMu.Unlock()
		return nil
	}

	a.applyUserConfig(nc, activeUsers)
	log.Printf("Node config applied: %d active users", len(activeUsers))
	return nil
}

// usersChanged compares two active user lists by UUID, speed limit and
// connection/device limits.
func usersChanged(oldUsers, newUsers []api.NodeUser) bool {
	if len(oldUsers) != len(newUsers) {
		return true
	}
	type userKey struct {
		speedLimit     int
		maxConnections int
		deviceLimit    int
	}
	oldMap := make(map[string]userKey, len(oldUsers))
	for _, u := range oldUsers {
		oldMap[u.UUID] = userKey{u.SpeedLimit, u.MaxConnections, u.DeviceLimit}
	}
	for _, u := range newUsers {
		if oldMap[u.UUID] != (userKey{u.SpeedLimit, u.MaxConnections, u.DeviceLimit}) {
			return true
		}
	}
	return false
}

// applyUserConfig hot-applies the user list and per-user limits in-process.
func (a *Agent) applyUserConfig(nc *api.NodeConfigResponse, activeUsers []api.NodeUser) {
	// 1. Update the inbound user list (no restart needed).
	if err := a.updateInboundUsers(nc.Protocols, activeUsers); err != nil {
		log.Printf("Failed to update users: %v", err)
	}

	a.usersMu.RLock()
	oldUsers := a.cachedUsers
	a.usersMu.RUnlock()

	// 2. Per-user speed limits (Mbps -> bytes/sec, keyed by UUID).
	active := make(map[string]bool, len(activeUsers))
	for _, u := range activeUsers {
		active[u.UUID] = true
		a.rateLimiter.SetLimit(u.UUID, int64(u.SpeedLimit)*1024*1024/8)
	}
	// Remove limits of users that are gone (removed or newly blocked).
	for _, u := range oldUsers {
		if !active[u.UUID] {
			a.rateLimiter.SetLimit(u.UUID, 0)
			// Kick their live connections too: a removed UUID (e.g. after a
			// subscription reset) must stop working immediately.
			if closed, err := a.trafficManager.CloseConnectionsByUser(u.UUID); err == nil && closed > 0 {
				log.Printf("Closed %d connections for removed user %s", closed, u.UUID)
			}
		}
	}

	// 3. Per-user connection/device limits, enforced at connection time.
	limits := make(map[string]trafficcontrol.UserLimit)
	for _, u := range activeUsers {
		if u.MaxConnections > 0 || u.DeviceLimit > 0 {
			limits[u.UUID] = trafficcontrol.UserLimit{
				MaxConnections: u.MaxConnections,
				MaxDevices:     u.DeviceLimit,
			}
		}
	}
	a.trafficManager.SetUserLimits(limits)

	a.usersMu.Lock()
	a.cachedUsers = activeUsers
	a.nodeConfig = nc
	a.usersMu.Unlock()
}

// updateInboundUsers pushes the active user list to the running inbound.
func (a *Agent) updateInboundUsers(protocols map[string]interface{}, users []api.NodeUser) error {
	tag := inboundTag(protocols)
	inbound, loaded := a.inboundManager.Get(tag)
	if !loaded {
		return fmt.Errorf("inbound %s not found", tag)
	}
	updatable, ok := inbound.(adapter.UserUpdatableInbound)
	if !ok {
		return fmt.Errorf("inbound %s does not support runtime user updates", tag)
	}

	flow := ""
	if a.realityKeys != nil {
		// VLESS+Reality users must keep the vision flow, otherwise auth fails
		flow = "xtls-rprx-vision"
	}
	entries := make([]adapter.UserEntry, 0, len(users))
	for _, u := range users {
		password := u.Password
		if password == "" {
			password = u.UUID
		}
		entries = append(entries, adapter.UserEntry{
			Name:     u.UUID, // unique per user, used for rate limit matching
			UUID:     u.UUID,
			Password: password,
			Flow:     flow,
		})
	}
	return updatable.UpdateUsers(entries)
}

// syncBlacklist polls the panel blacklist and applies changes: newly blocked
// users are removed from the inbound and their connections closed; restored
// users are added back.
func (a *Agent) syncBlacklist() error {
	list, err := a.apiClient.Blacklist()
	if err != nil {
		return fmt.Errorf("blacklist sync failed: %w", err)
	}

	newBlocked := make(map[string]bool, len(list.Blocked))
	for _, u := range list.Blocked {
		newBlocked[u.UUID] = true
	}

	// Apply the user+IP block list: reject reconnects in the traffic manager
	// and kick connections of newly blocked devices.
	newDevices := make(map[string]bool, len(list.BlockedDevices))
	pairs := make([][2]string, 0, len(list.BlockedDevices))
	for _, d := range list.BlockedDevices {
		key := d.UUID + "|" + d.IP
		newDevices[key] = true
		pairs = append(pairs, [2]string{d.UUID, d.IP})
	}
	a.trafficManager.SetBlockedDevices(pairs)

	a.blockListMu.Lock()
	oldDevices := a.blockedDevices
	a.blockedDevices = newDevices
	oldBlocked := a.blockedUUIDs
	a.blockedUUIDs = newBlocked
	a.blockListMu.Unlock()

	for _, d := range list.BlockedDevices {
		key := d.UUID + "|" + d.IP
		if !oldDevices[key] {
			if closed := a.trafficManager.CloseConnectionsByUserIP(d.UUID, d.IP); closed > 0 {
				log.Printf("Kicked blocked device: user %s ip %s (%d connections)", d.UUID, d.IP, closed)
			}
		}
	}

	var added []string
	for uuid := range newBlocked {
		if !oldBlocked[uuid] {
			added = append(added, uuid)
			log.Printf("New blocked UUID detected: %s", uuid)
		}
	}
	changed := len(added) > 0
	for uuid := range oldBlocked {
		if !newBlocked[uuid] {
			changed = true
			log.Printf("Blocked UUID removed (user restored): %s", uuid)
		}
	}
	if !changed {
		return nil
	}

	// Re-fetch the full user list, apply without the blocked users.
	nc, err := a.apiClient.NodeConfig()
	if err != nil {
		return fmt.Errorf("fetch node config for blacklist update: %w", err)
	}
	activeUsers := filterBlocked(nc.Users, newBlocked)
	if err := a.updateInboundUsers(nc.Protocols, activeUsers); err != nil {
		log.Printf("Failed to update users: %v", err)
	} else {
		log.Printf("Updated sing-box users: %d active", len(activeUsers))
	}

	// Close connections of newly blocked users.
	for _, uuid := range added {
		closed, err := a.trafficManager.CloseConnectionsByUser(uuid)
		if err != nil {
			log.Printf("Failed to close connections for user %s: %v", uuid, err)
		} else {
			log.Printf("Closed %d connections for blocked user %s", closed, uuid)
		}
		a.rateLimiter.SetLimit(uuid, 0)
	}

	a.usersMu.Lock()
	a.cachedUsers = activeUsers
	a.nodeConfig = nc
	a.usersMu.Unlock()

	log.Printf("Blacklist synced: %d blocked UUIDs", len(newBlocked))
	return nil
}

// reportTraffic computes per-user traffic deltas against the last snapshot
// and posts them to the panel.
func (a *Agent) reportTraffic() error {
	current := a.trafficManager.UserTraffic()

	a.usersMu.RLock()
	nc := a.nodeConfig
	a.usersMu.RUnlock()
	if nc == nil || nc.NodeID == 0 {
		var err error
		nc, err = a.apiClient.NodeConfig()
		if err != nil {
			return fmt.Errorf("traffic report failed: %w", err)
		}
	}
	uuidToUserID := make(map[string]int64, len(nc.Users))
	for _, u := range nc.Users {
		uuidToUserID[u.UUID] = u.UserID
	}

	a.trafficMu.Lock()
	prev := a.lastTraffic
	a.lastTraffic = current
	a.trafficMu.Unlock()

	records := make([]api.TrafficRecord, 0, len(current))
	for uuid, cur := range current {
		userID := uuidToUserID[uuid]
		if userID == 0 {
			continue
		}
		p := prev[uuid]
		up, down := cur[0]-p[0], cur[1]-p[1]
		if up < 0 {
			up = cur[0] // core restarted, counters reset
		}
		if down < 0 {
			down = cur[1]
		}
		if up == 0 && down == 0 {
			continue
		}
		records = append(records, api.TrafficRecord{
			UserID:   userID,
			NodeID:   nc.NodeID,
			Upload:   up,
			Download: down,
		})
	}
	if len(records) == 0 {
		return nil
	}
	if err := a.apiClient.ReportTraffic(records); err != nil {
		return fmt.Errorf("traffic report failed: %w", err)
	}
	log.Printf("Reported traffic for %d users", len(records))
	return nil
}

// reportDevices posts the per-user online device list (distinct source IPs
// with active connections) to the panel.
func (a *Agent) reportDevices() error {
	devices := a.trafficManager.UserDevices()
	if len(devices) == 0 {
		return nil
	}

	a.usersMu.RLock()
	nc := a.nodeConfig
	a.usersMu.RUnlock()
	if nc == nil || nc.NodeID == 0 {
		var err error
		nc, err = a.apiClient.NodeConfig()
		if err != nil {
			return fmt.Errorf("device report failed: %w", err)
		}
	}

	records := make([]api.DeviceRecord, 0, len(devices))
	for uuid, ips := range devices {
		records = append(records, api.DeviceRecord{UUID: uuid, IPs: ips})
	}
	if err := a.apiClient.ReportDevices(api.DeviceReport{NodeID: nc.NodeID, Devices: records}); err != nil {
		return fmt.Errorf("device report failed: %w", err)
	}
	return nil
}
