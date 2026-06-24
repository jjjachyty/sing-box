package ratelimiter

import (
	"io"
	"net"
	"sync"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/juju/ratelimit"
)

// Manager 全局限速管理器，按用户 name 维护 token bucket
type Manager struct {
	mu      sync.RWMutex
	buckets map[string]*ratelimit.Bucket
}

// NewManager 创建新的限速管理器
func NewManager() *Manager {
	return &Manager{
		buckets: make(map[string]*ratelimit.Bucket),
	}
}

// SetLimit 设置指定用户 name 的限速（字节/秒），limit <= 0 表示取消限速
func (m *Manager) SetLimit(name string, bytesPerSec int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if bytesPerSec <= 0 {
		delete(m.buckets, name)
		return
	}
	m.buckets[name] = ratelimit.NewBucketWithQuantum(
		1,              // 填充周期 1秒
		bytesPerSec*2,  // 容量
		bytesPerSec,    // 每周期填充量
	)
}

// GetLimit 获取用户 name 的限速（字节/秒），0 表示无限制
func (m *Manager) GetLimit(name string) int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.buckets[name]
	if !ok {
		return 0
	}
	return int64(b.Rate())
}

// GetLimits 返回所有限速配置
func (m *Manager) GetLimits() map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]int64, len(m.buckets))
	for name, b := range m.buckets {
		result[name] = int64(b.Rate())
	}
	return result
}

// WrapConn 包装 net.Conn 加入限速
func (m *Manager) WrapConn(conn net.Conn, name string) net.Conn {
	m.mu.RLock()
	b, ok := m.buckets[name]
	m.mu.RUnlock()
	if !ok {
		return conn
	}
	return &rateLimitedConn{Conn: conn, bucket: b}
}

// WrapPacketConn 包装 N.PacketConn 加入限速（在 ReadPacket/WritePacket 级别）
func (m *Manager) WrapPacketConn(conn N.PacketConn, name string) N.PacketConn {
	m.mu.RLock()
	b, ok := m.buckets[name]
	m.mu.RUnlock()
	if !ok {
		return conn
	}
	return &rateLimitedPacketConn{PacketConn: conn, bucket: b}
}

// rateLimitedConn TCP 限速连接包装器
type rateLimitedConn struct {
	net.Conn
	bucket *ratelimit.Bucket
}

func (c *rateLimitedConn) Read(p []byte) (n int, err error) {
	n, err = c.Conn.Read(p)
	if n > 0 {
		c.bucket.Wait(int64(n))
	}
	return
}

func (c *rateLimitedConn) Write(p []byte) (n int, err error) {
	c.bucket.Wait(int64(len(p)))
	return c.Conn.Write(p)
}

var _ net.Conn = (*rateLimitedConn)(nil)

// rateLimitedPacketConn UDP 限速连接包装器
type rateLimitedPacketConn struct {
	N.PacketConn
	bucket *ratelimit.Bucket
}

func (c *rateLimitedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	addr, err := c.PacketConn.ReadPacket(buffer)
	if err == nil && buffer.Len() > 0 {
		c.bucket.Wait(int64(buffer.Len()))
	}
	return addr, err
}

func (c *rateLimitedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	c.bucket.Wait(int64(buffer.Len()))
	return c.PacketConn.WritePacket(buffer, destination)
}

var _ N.PacketConn = (*rateLimitedPacketConn)(nil)

// helper: limit for io.Copy 限速 reader
func (m *Manager) LimitReader(r io.Reader, name string) io.Reader {
	m.mu.RLock()
	b, ok := m.buckets[name]
	m.mu.RUnlock()
	if !ok {
		return r
	}
	return &rateLimitedReader{reader: r, bucket: b}
}

type rateLimitedReader struct {
	reader io.Reader
	bucket *ratelimit.Bucket
}

func (r *rateLimitedReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	if n > 0 {
		r.bucket.Wait(int64(n))
	}
	return
}

// helper: limit for io.Copy 限速 writer
func (m *Manager) LimitWriter(w io.Writer, name string) io.Writer {
	m.mu.RLock()
	b, ok := m.buckets[name]
	m.mu.RUnlock()
	if !ok {
		return w
	}
	return &rateLimitedWriter{writer: w, bucket: b}
}

type rateLimitedWriter struct {
	writer io.Writer
	bucket *ratelimit.Bucket
}

func (w *rateLimitedWriter) Write(p []byte) (n int, err error) {
	w.bucket.Wait(int64(len(p)))
	return w.writer.Write(p)
}
