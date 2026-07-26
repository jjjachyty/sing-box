package trafficcontrol

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/compatible"
	"github.com/sagernet/sing/common/cleanup"
	"github.com/sagernet/sing/common/observable"
	"github.com/sagernet/sing/common/x/list"

	"github.com/gofrs/uuid/v5"
)

type ConnectionEventType int

const (
	ConnectionEventNew ConnectionEventType = iota
	ConnectionEventClosed
)

type ConnectionEvent struct {
	Type     ConnectionEventType
	ID       uuid.UUID
	Metadata *TrackerMetadata
	ClosedAt time.Time
}

const closedConnectionsLimit = 1000

var (
	_ adapter.ConnectionTracker = (*Manager)(nil)
	_ adapter.LifecycleService  = (*Manager)(nil)
)

type Manager struct {
	outbound      adapter.OutboundManager
	uploadTotal   atomic.Int64
	downloadTotal atomic.Int64

	connections             compatible.Map[uuid.UUID, Tracker]
	closedConnectionsAccess sync.Mutex
	closedConnections       list.List[TrackerMetadata]

	// NEW: per-user traffic accumulator, keyed by Metadata.User
	userTraffic sync.Map

	// NEW: per-user connection/device limits, pushed by the node agent
	userLimits  sync.Map // user string -> UserLimit
	userConns   sync.Map // user string -> *atomic.Int64 (active tracked connections)
	userDevices sync.Map // user string -> *userDeviceSet (active connections per source IP)

	// NEW: user+IP block list (devices removed by the user on the panel),
	// key format "user|ip"
	blockedDevices sync.Map

	eventSubscriber *observable.Subscriber[ConnectionEvent]
	eventObserver   *observable.Observer[ConnectionEvent]
	cleaner         *cleanup.Cleaner
}

// UserLimit defines per-user caps enforced at connection time.
// A zero value means no limit for that dimension.
type UserLimit struct {
	MaxConnections int // max concurrent tracked connections, 0 = unlimited
	MaxDevices     int // max distinct source IPs with active connections, 0 = unlimited
}

// userDeviceSet counts active connections per source IP for one user.
type userDeviceSet struct {
	mu  sync.Mutex
	ips map[string]int64
}

// SetUserLimits replaces the whole per-user limit table. Users absent from
// the map are unrestricted; their counters are reset.
func (m *Manager) SetUserLimits(limits map[string]UserLimit) {
	m.userLimits.Range(func(key, _ any) bool {
		user := key.(string)
		if _, ok := limits[user]; !ok {
			m.userLimits.Delete(user)
			m.userConns.Delete(user)
			m.userDevices.Delete(user)
		}
		return true
	})
	for user, limit := range limits {
		m.userLimits.Store(user, limit)
	}
}

// SetBlockedDevices replaces the user+IP block list (full replace).
func (m *Manager) SetBlockedDevices(pairs [][2]string) {
	next := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		next[p[0]+"|"+p[1]] = true
	}
	m.blockedDevices.Range(func(key, _ any) bool {
		if !next[key.(string)] {
			m.blockedDevices.Delete(key)
		}
		return true
	})
	for key := range next {
		m.blockedDevices.Store(key, struct{}{})
	}
}

// isDeviceBlocked reports whether this user+IP combination is blocked.
func (m *Manager) isDeviceBlocked(user, sourceIP string) bool {
	if user == "" || sourceIP == "" {
		return false
	}
	_, ok := m.blockedDevices.Load(user + "|" + sourceIP)
	return ok
}

// CloseConnectionsByUserIP closes all active connections of user that come
// from sourceIP, returning how many were closed.
func (m *Manager) CloseConnectionsByUserIP(user, sourceIP string) int {
	closed := 0
	m.connections.Range(func(_ uuid.UUID, tracker Tracker) bool {
		md := tracker.Metadata()
		if md.Metadata.User == user && md.Metadata.Source.AddrString() == sourceIP {
			tracker.Close()
			closed++
		}
		return true
	})
	return closed
}

// allowConnection reports whether a new tracked connection for user from
// sourceIP may proceed. Enforcement is best-effort: the check and the join
// increment are not atomic, so bursts can briefly overshoot by one.
func (m *Manager) allowConnection(user, sourceIP string) bool {
	if m.isDeviceBlocked(user, sourceIP) {
		return false
	}
	raw, ok := m.userLimits.Load(user)
	if !ok {
		return true
	}
	limit := raw.(UserLimit)
	if limit.MaxConnections > 0 {
		if rawCount, loaded := m.userConns.Load(user); loaded {
			if rawCount.(*atomic.Int64).Load() >= int64(limit.MaxConnections) {
				return false
			}
		}
	}
	if limit.MaxDevices > 0 && sourceIP != "" {
		if rawSet, loaded := m.userDevices.Load(user); loaded {
			set := rawSet.(*userDeviceSet)
			set.mu.Lock()
			_, known := set.ips[sourceIP]
			devices := len(set.ips)
			set.mu.Unlock()
			if !known && devices >= limit.MaxDevices {
				return false
			}
		}
	}
	return true
}

// trackJoin counts a new active connection for its user.
func (m *Manager) trackJoin(metadata *TrackerMetadata) {
	user := metadata.Metadata.User
	if user == "" {
		return
	}
	rawCount, _ := m.userConns.LoadOrStore(user, new(atomic.Int64))
	rawCount.(*atomic.Int64).Add(1)
	ip := metadata.Metadata.Source.AddrString()
	if ip != "" {
		rawSet, _ := m.userDevices.LoadOrStore(user, &userDeviceSet{ips: make(map[string]int64)})
		set := rawSet.(*userDeviceSet)
		set.mu.Lock()
		set.ips[ip]++
		set.mu.Unlock()
	}
}

// trackLeave releases the counters held by a closed connection.
func (m *Manager) trackLeave(metadata *TrackerMetadata) {
	user := metadata.Metadata.User
	if user == "" {
		return
	}
	if rawCount, ok := m.userConns.Load(user); ok {
		counter := rawCount.(*atomic.Int64)
		if n := counter.Add(-1); n < 0 {
			counter.Store(0)
		}
	}
	ip := metadata.Metadata.Source.AddrString()
	if ip != "" {
		if rawSet, ok := m.userDevices.Load(user); ok {
			set := rawSet.(*userDeviceSet)
			set.mu.Lock()
			if n := set.ips[ip] - 1; n <= 0 {
				delete(set.ips, ip)
			} else {
				set.ips[ip] = n
			}
			set.mu.Unlock()
		}
	}
}

func NewManager(outbound adapter.OutboundManager) *Manager {
	manager := &Manager{
		outbound:        outbound,
		eventSubscriber: observable.NewSubscriber[ConnectionEvent](256),
	}
	manager.eventObserver = observable.NewObserver(manager.eventSubscriber, 64)
	manager.cleaner = cleanup.Add(manager.Clear)
	return manager
}

func (m *Manager) Name() string {
	return "traffic manager"
}

func (m *Manager) Start(stage adapter.StartStage) error {
	return nil
}

func (m *Manager) Close() error {
	m.cleaner.Close()
	return m.eventObserver.Close()
}

func (m *Manager) SubscribeEvents() (observable.Subscription[ConnectionEvent], <-chan struct{}, error) {
	return m.eventObserver.Subscribe()
}

func (m *Manager) UnSubscribeEvents(subscription observable.Subscription[ConnectionEvent]) {
	m.eventObserver.UnSubscribe(subscription)
}

func (m *Manager) join(tracker Tracker) {
	metadata := tracker.Metadata()
	m.connections.Store(metadata.ID, tracker)
	m.trackJoin(metadata)
	m.eventSubscriber.Emit(ConnectionEvent{
		Type:     ConnectionEventNew,
		ID:       metadata.ID,
		Metadata: metadata,
	})
}

func (m *Manager) leave(tracker Tracker) {
	metadata := tracker.Metadata()
	_, loaded := m.connections.LoadAndDelete(metadata.ID)
	if !loaded {
		return
	}
	m.trackLeave(metadata)
	closedAt := time.Now()
	metadata.ClosedAt = closedAt
	// NEW: fold the closed connection's bytes into the per-user accumulator
	if user := metadata.Metadata.User; user != "" {
		m.addUserTraffic(user, metadata.Upload.Load(), metadata.Download.Load())
	}
	metadataCopy := *metadata
	m.closedConnectionsAccess.Lock()
	if m.closedConnections.Len() >= closedConnectionsLimit {
		m.closedConnections.PopFront()
	}
	m.closedConnections.PushBack(metadataCopy)
	m.closedConnectionsAccess.Unlock()
	m.eventSubscriber.Emit(ConnectionEvent{
		Type:     ConnectionEventClosed,
		ID:       metadata.ID,
		Metadata: &metadataCopy,
		ClosedAt: closedAt,
	})
}

func (m *Manager) Total() (uplinkTotal int64, downlinkTotal int64) {
	return m.uploadTotal.Load(), m.downloadTotal.Load()
}

// userTrafficCounter accumulates bytes of closed connections for one user.
type userTrafficCounter struct {
	upload   atomic.Int64
	download atomic.Int64
}

func (m *Manager) addUserTraffic(user string, upload, download int64) {
	v, _ := m.userTraffic.LoadOrStore(user, &userTrafficCounter{})
	c := v.(*userTrafficCounter)
	c.upload.Add(upload)
	c.download.Add(download)
}

// UserTraffic returns per-user traffic totals (upload, download) since
// process start: bytes of closed connections plus bytes still in flight
// on active connections.
func (m *Manager) UserTraffic() map[string][2]int64 {
	result := make(map[string][2]int64)
	m.userTraffic.Range(func(key, value any) bool {
		c := value.(*userTrafficCounter)
		result[key.(string)] = [2]int64{c.upload.Load(), c.download.Load()}
		return true
	})
	m.connections.Range(func(_ uuid.UUID, tracker Tracker) bool {
		md := tracker.Metadata()
		if user := md.Metadata.User; user != "" {
			t := result[user]
			t[0] += md.Upload.Load()
			t[1] += md.Download.Load()
			result[user] = t
		}
		return true
	})
	return result
}

// OnlineUsers returns the number of distinct users with at least one
// active connection.
func (m *Manager) OnlineUsers() int {
	seen := make(map[string]bool)
	m.connections.Range(func(_ uuid.UUID, tracker Tracker) bool {
		if user := tracker.Metadata().Metadata.User; user != "" {
			seen[user] = true
		}
		return true
	})
	return len(seen)
}

func (m *Manager) ConnectionsLen() int {
	return m.connections.Len()
}

// UserDevices returns the distinct source IPs with at least one active
// connection, keyed by user. Used by the node agent to report online devices.
func (m *Manager) UserDevices() map[string][]string {
	result := make(map[string][]string)
	m.userDevices.Range(func(key, value any) bool {
		set := value.(*userDeviceSet)
		set.mu.Lock()
		ips := make([]string, 0, len(set.ips))
		for ip := range set.ips {
			ips = append(ips, ip)
		}
		set.mu.Unlock()
		if len(ips) > 0 {
			result[key.(string)] = ips
		}
		return true
	})
	return result
}

func (m *Manager) Connections() []*TrackerMetadata {
	var connections []*TrackerMetadata
	m.connections.Range(func(_ uuid.UUID, tracker Tracker) bool {
		connections = append(connections, tracker.Metadata())
		return true
	})
	return connections
}

func (m *Manager) ClosedConnections() []*TrackerMetadata {
	m.closedConnectionsAccess.Lock()
	values := m.closedConnections.Array()
	m.closedConnectionsAccess.Unlock()
	if len(values) == 0 {
		return nil
	}
	connections := make([]*TrackerMetadata, len(values))
	for i := range values {
		connections[i] = &values[i]
	}
	return connections
}

func (m *Manager) Connection(id uuid.UUID) Tracker {
	connection, loaded := m.connections.Load(id)
	if !loaded {
		return nil
	}
	return connection
}

func (m *Manager) CloseAllConnections() {
	m.connections.Range(func(_ uuid.UUID, tracker Tracker) bool {
		tracker.Close()
		return true
	})
}

// CloseConnectionsByUser closes all active connections where Metadata.User == user.
func (m *Manager) CloseConnectionsByUser(user string) (int, error) {
	var closed int
	m.connections.Range(func(_ uuid.UUID, tracker Tracker) bool {
		if tracker.Metadata().Metadata.User == user {
			tracker.Close()
			closed++
		}
		return true
	})
	return closed, nil
}

func (m *Manager) Clear() {
	m.closedConnectionsAccess.Lock()
	defer m.closedConnectionsAccess.Unlock()
	m.closedConnections.Init()
}
