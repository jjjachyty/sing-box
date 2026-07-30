package libbox

// https://github.com/SagerNet/sing-box/issues/3233
// https://github.com/golang/go/issues/70508
// https://github.com/tailscale/tailscale/issues/13452
//
// os.checkPidfdOnce was removed in Go 1.26 (pidfd is used unconditionally),
// so the go:linkname hack is no longer needed or linkable.
