package forwardproxy

import (
	"encoding/base64"
	"log"
	"strconv"
	"strings"

	caddy "github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"net"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("forward_proxy", parseCaddyfile)
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var fp Handler
	err := fp.UnmarshalCaddyfile(h.Dispenser)
	return &fp, err
}

// UnmarshalCaddyfile unmarshals Caddyfile tokens into h.
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if !d.Next() {
		return d.ArgErr()
	}
	args := d.RemainingArgs()
	if len(args) > 0 {
		return d.ArgErr()
	}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		subdirective := d.Val()
		args := d.RemainingArgs()
		switch subdirective {
		case "basic_auth":
			if len(args) != 2 {
				return d.ArgErr()
			}
			if len(args[0]) == 0 {
				return d.Err("empty usernames are not allowed")
			}
			// TODO: Evaluate policy of allowing empty passwords.
			if strings.Contains(args[0], ":") {
				return d.Err("character ':' in usernames is not allowed")
			}
			if h.AuthCredentials == nil {
				h.AuthCredentials = [][]byte{}
			}
			// base64-encode credentials
			buf := make([]byte, base64.StdEncoding.EncodedLen(len(args[0])+1+len(args[1])))
			base64.StdEncoding.Encode(buf, []byte(args[0]+":"+args[1]))
			h.AuthCredentials = append(h.AuthCredentials, buf)
			h.AuthRequired = true
		case "hosts":
			if len(args) == 0 {
				return d.ArgErr()
			}
			if len(h.Hosts) != 0 {
				return d.Err("hosts subdirective specified twice")
			}
			h.Hosts = caddyhttp.MatchHost(args)
		case "ports":
			if len(args) == 0 {
				return d.ArgErr()
			}
			if len(h.AllowedPorts) != 0 {
				return d.Err("ports subdirective specified twice")
			}
			h.AllowedPorts = make([]int, len(args))
			for i, p := range args {
				intPort, err := strconv.Atoi(p)
				if intPort <= 0 || intPort > 65535 || err != nil {
					return d.Errf("ports are expected to be space-separated and in 0-65535 range, but got: %s", p)
				}
				h.AllowedPorts[i] = intPort
			}
		case "hide_ip":
			if len(args) != 0 {
				return d.ArgErr()
			}
			h.HideIP = true
		case "hide_via":
			if len(args) != 0 {
				return d.ArgErr()
			}
			h.HideVia = true
		case "probe_resistance":
			if len(args) > 1 {
				return d.ArgErr()
			}
			if len(args) == 1 {
				lowercaseArg := strings.ToLower(args[0])
				if lowercaseArg != args[0] {
					log.Println("[WARNING] Secret domain appears to have uppercase letters in it, which are not visitable")
				}
				h.ProbeResistance = &ProbeResistance{Domain: args[0]}
			} else {
				h.ProbeResistance = &ProbeResistance{}
			}
		case "serve_pac":
			if len(args) > 1 {
				return d.ArgErr()
			}
			if len(h.PACPath) != 0 {
				return d.Err("serve_pac subdirective specified twice")
			}
			if len(args) == 1 {
				h.PACPath = args[0]
				if !strings.HasPrefix(h.PACPath, "/") {
					h.PACPath = "/" + h.PACPath
				}
			} else {
				h.PACPath = "/proxy.pac"
			}
		case "dial_timeout":
			if len(args) != 1 {
				return d.ArgErr()
			}
			timeout, err := caddy.ParseDuration(args[0])
			if err != nil {
				return d.ArgErr()
			}
			if timeout < 0 {
				return d.Err("dial_timeout cannot be negative.")
			}
			h.DialTimeout = caddy.Duration(timeout)
		case "upstream":
			if len(args) != 1 {
				return d.ArgErr()
			}
			if h.Upstream != "" {
				return d.Err("upstream directive specified more than once")
			}
			h.Upstream = args[0]
		case "acl":
			for nesting := d.Nesting(); d.NextBlock(nesting); {
				aclDirective := d.Val()
				args := d.RemainingArgs()
				if len(args) == 0 {
					return d.ArgErr()
				}
				var ruleSubjects []string
				var err error
				aclAllow := false
				switch aclDirective {
				case "allow":
					ruleSubjects = args
					aclAllow = true
				case "allow_file":
					if len(args) != 1 {
						return d.Err("allowfile accepts a single filename argument")
					}
					ruleSubjects, err = readLinesFromFile(args[0])
					if err != nil {
						return err
					}
					aclAllow = true
				case "deny":
					ruleSubjects = args
				case "deny_file":
					if len(args) != 1 {
						return d.Err("denyfile accepts a single filename argument")
					}
					ruleSubjects, err = readLinesFromFile(args[0])
					if err != nil {
						return err
					}
				default:
					return d.Err("expected acl directive: allow/allowfile/deny/denyfile." +
						"got: " + aclDirective)
				}
				ar := ACLRule{Subjects: ruleSubjects, Allow: aclAllow}
				h.ACL = append(h.ACL, ar)
			}
		case "bind":
			if len(args) != 1 {
				return d.ArgErr()
			}
			if _, addrnet, err := net.ParseCIDR(args[0]); err == nil {
				ones, bits := addrnet.Mask.Size()
				if bits == 0 || ones&7 != 0 {
					// TODO: support more subnet sizes
					return d.Err("unsupported subnet to bind to: size must be a multiple of 8")
				}
				h.DefaultBind = addrnet
			} else if ip := net.ParseIP(args[0]); ip != nil {
				bits := len(ip) << 3
				h.DefaultBind = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
			} else {
				return err
			}
		case "host_override":
			if len(args) != 2 {
				return d.ArgErr()
			}
			if h.HostOverride == nil {
				h.HostOverride = make(map[string]string)
			}
			h.HostOverride[strings.ToLower(args[0])] = args[1]
		default:
			return d.ArgErr()
		}
	}
	return nil
}
