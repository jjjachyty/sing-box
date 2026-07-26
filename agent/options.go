package agent

import (
	"context"
	"log"
	"net"
	"strconv"

	"github.com/sagernet/sing-box/agent/api"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
)

// protocolBool reads a boolean flag from the node protocols config,
// accepting both JSON bool and string forms ("true", "1", "yes").
func protocolBool(protocols map[string]interface{}, key string) bool {
	switch v := protocols[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1" || v == "yes"
	}
	return false
}

// protocolType returns the inbound protocol type from the node config.
func protocolType(protocols map[string]interface{}) string {
	protoType, _ := protocols["type"].(string)
	if protoType == "" {
		protoType = "vmess"
	}
	return protoType
}

// inboundTag returns the sing-box inbound tag based on the protocol type.
func inboundTag(protocols map[string]interface{}) string {
	switch protocolType(protocols) {
	case "vless":
		return "vless-in"
	case "ss":
		return "ss-in"
	case "trojan":
		return "trojan-in"
	case "hysteria2":
		return "hysteria2-in"
	default:
		return "vmess-in"
	}
}

// buildOptions generates the sing-box options from the node config and the
// active (non-blocked) user list. The JSON structure matches what the
// external agent used to write to config.json, except the clash API is kept
// with an empty external_controller: box.New then creates the traffic
// manager and rate limiter (needed for in-process control) without listening
// on any port.
func buildOptions(ctx context.Context, nc *api.NodeConfigResponse, users []api.NodeUser, realityKeys *realityKeyPair) (option.Options, error) {
	config := map[string]interface{}{
		"log": map[string]interface{}{
			"level":  "info",
			"output": "logs/sing-box.log",
		},
		"dns": map[string]interface{}{
			"servers": []map[string]interface{}{
				{"type": "udp", "tag": "google", "server": "8.8.8.8"},
				{"type": "udp", "tag": "local", "server": "223.5.5.5"},
			},
		},
		"inbounds": []map[string]interface{}{},
		"outbounds": []map[string]interface{}{
			{"type": "direct", "tag": "direct"},
			{"type": "block", "tag": "blocked"},
		},
		"route": map[string]interface{}{
			"rules": []map[string]interface{}{
				{"network": "tcp,udp", "outbound": "direct"},
			},
			"final":                   "direct",
			"default_domain_resolver": "local",
		},
		"experimental": map[string]interface{}{
			// NEW: empty external_controller => clash API services are created
			// (traffic manager, rate limiter) but nothing listens on a port.
			"clash_api": map[string]interface{}{
				"external_controller": "",
			},
		},
	}

	// Users are keyed by UUID as their name: the forked sing-box matches the
	// authenticated user name against per-user rate limit buckets,
	// so a unique name per user gives true per-user speed limits.
	uuidUsers := make([]map[string]interface{}, 0, len(users))
	// trojan / shadowsocks / hysteria2 users authenticate by password
	passwordUsers := make([]map[string]interface{}, 0, len(users))
	for _, u := range users {
		uuidUsers = append(uuidUsers, map[string]interface{}{
			"name": u.UUID,
			"uuid": u.UUID,
		})
		password := u.Password
		if password == "" {
			password = u.UUID
		}
		passwordUsers = append(passwordUsers, map[string]interface{}{
			"name":     u.UUID,
			"password": password,
		})
	}

	inbounds := []map[string]interface{}{}
	addInbound := func(inbound map[string]interface{}, tlsRequired bool) {
		// hysteria2 always needs TLS; other protocols when the panel says so.
		// Cert paths come from the node protocols config; otherwise a
		// self-signed certificate is generated in the working directory.
		needTLS := tlsRequired || protocolBool(nc.Protocols, "tls")
		if needTLS {
			if tlsCfg := tlsInboundConfig(nc); tlsCfg != nil {
				inbound["tls"] = tlsCfg
			}
		}
		inbounds = append(inbounds, inbound)
	}
	switch protocolType(nc.Protocols) {
	case "vmess":
		addInbound(map[string]interface{}{
			"type":        "vmess",
			"tag":         "vmess-in",
			"listen":      "0.0.0.0",
			"listen_port": nc.Port,
			"users":       uuidUsers,
		}, false)
	case "vless":
		inbound := map[string]interface{}{
			"type":        "vless",
			"tag":         "vless-in",
			"listen":      "0.0.0.0",
			"listen_port": nc.Port,
			"users":       uuidUsers,
		}
		if realityKeys != nil {
			// VLESS + Reality: no certificate, TLS handshake is borrowed from
			// the camouflage target; clients must use xtls-rprx-vision flow.
			for _, u := range uuidUsers {
				u["flow"] = "xtls-rprx-vision"
			}
			inbound["tls"] = realityTLSConfig(nc, realityKeys)
			inbounds = append(inbounds, inbound)
		} else {
			addInbound(inbound, false)
		}
	case "ss":
		method := "aes-256-gcm"
		if s, ok := nc.Protocols["cipher"].(string); ok {
			method = s
		}
		addInbound(map[string]interface{}{
			"type":        "shadowsocks",
			"tag":         "ss-in",
			"listen":      "0.0.0.0",
			"listen_port": nc.Port,
			"method":      method,
			"users":       passwordUsers,
		}, false)
	case "trojan":
		addInbound(map[string]interface{}{
			"type":        "trojan",
			"tag":         "trojan-in",
			"listen":      "0.0.0.0",
			"listen_port": nc.Port,
			"users":       passwordUsers,
		}, false)
	case "hysteria2":
		addInbound(map[string]interface{}{
			"type":        "hysteria2",
			"tag":         "hysteria2-in",
			"listen":      "0.0.0.0",
			"listen_port": nc.Port,
			"users":       passwordUsers,
		}, true)
	default:
		addInbound(map[string]interface{}{
			"type":        "vmess",
			"tag":         "vmess-in",
			"listen":      "0.0.0.0",
			"listen_port": nc.Port,
			"users":       uuidUsers,
		}, false)
	}
	config["inbounds"] = inbounds

	content, err := json.Marshal(config)
	if err != nil {
		return option.Options{}, err
	}
	// Decode through the same extended-JSON path cmd_run.go uses for config
	// files, so we do not hand-assemble deep option structures.
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, content)
	if err != nil {
		return option.Options{}, err
	}
	return options, nil
}

// tlsInboundConfig returns TLS settings for inbounds that require it.
// Cert paths come from the node protocols config (cert_path / key_path);
// if absent, a self-signed certificate is generated once in ./certs.
func tlsInboundConfig(nc *api.NodeConfigResponse) map[string]interface{} {
	certPath, _ := nc.Protocols["cert_path"].(string)
	keyPath, _ := nc.Protocols["key_path"].(string)
	if certPath == "" || keyPath == "" {
		certPath = "certs/selfsigned.crt"
		keyPath = "certs/selfsigned.key"
		if err := ensureSelfSignedCert(certPath, keyPath); err != nil {
			log.Printf("Failed to generate self-signed cert: %v", err)
			return nil
		}
	}
	tlsCfg := map[string]interface{}{
		"enabled":          true,
		"certificate_path": certPath,
		"key_path":         keyPath,
	}
	if sni, _ := nc.Protocols["sni"].(string); sni != "" {
		tlsCfg["server_name"] = sni
	}
	return tlsCfg
}

// realityTLSConfig builds the inbound TLS block for VLESS+Reality.
// reality_dest / reality_server_name come from the node protocols config;
// the private key is always the node's local keypair.
func realityTLSConfig(nc *api.NodeConfigResponse, realityKeys *realityKeyPair) map[string]interface{} {
	serverName, _ := nc.Protocols["reality_server_name"].(string)
	if serverName == "" {
		serverName, _ = nc.Protocols["sni"].(string)
	}
	if serverName == "" {
		serverName = "www.apple.com"
	}
	dest, _ := nc.Protocols["reality_dest"].(string)
	if dest == "" {
		dest = serverName + ":443"
	}
	host := dest
	port := 443
	if h, p, err := net.SplitHostPort(dest); err == nil {
		host = h
		if v, err := strconv.Atoi(p); err == nil {
			port = v
		}
	}
	return map[string]interface{}{
		"enabled":     true,
		"server_name": serverName,
		"reality": map[string]interface{}{
			"enabled": true,
			"handshake": map[string]interface{}{
				"server":      host,
				"server_port": port,
			},
			"private_key": realityKeys.PrivateKey,
			"short_id":    []string{realityKeys.ShortID},
		},
	}
}
