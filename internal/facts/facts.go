// Package facts collects one read-only observation of a managed Linux node.
package facts

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"time"
)

type Snapshot struct {
	CollectedAt time.Time         `json:"collected_at"`
	Values      map[string]any    `json:"values"`
	Unavailable map[string]string `json:"unavailable,omitempty"`
}

func Collect(name string) Snapshot {
	return collect(name, os.ReadFile, os.Hostname, net.Interfaces, func(i net.Interface) ([]net.Addr, error) { return i.Addrs() }, fullyQualifiedName, virtualizationType)
}

// The ordinary OS entry points are passed here so failed observations can be
// tested without changing the machine or hiding failures behind zero values.
func collect(name string, read func(string) ([]byte, error), nodename func() (string, error), interfaces func() ([]net.Interface, error), addresses func(net.Interface) ([]net.Addr, error), qualified func(string) string, virtualization func() (string, error)) Snapshot {
	s := Snapshot{CollectedAt: time.Now().UTC(), Values: map[string]any{"name": name, "arch": runtime.GOARCH}, Unavailable: map[string]string{}}
	h, err := nodename()
	s.put("hostname", h, err)
	full := ""
	if err == nil {
		full = qualified(h)
	}
	s.put("fully_qualified_name", full, err)
	kind, err := virtualization()
	s.put("virtualization.type", kind, err)
	gatherSystem(&s, read)
	links, err := interfaces()
	if err != nil {
		s.put("network.interfaces", nil, err)
		s.put("network.default_routes", nil, err)
		s.put("network.default_ipv4_address", nil, err)
		s.put("network.default_ipv6_address", nil, err)
	} else {
		values := map[string]any{}
		for _, link := range links {
			item := map[string]any{"index": link.Index, "mtu": link.MTU, "mac": link.HardwareAddr.String()}
			values[link.Name] = item
			addrs, err := addresses(link)
			list := []any{}
			if err == nil {
				for _, a := range addrs {
					prefix, e := netip.ParsePrefix(a.String())
					if e != nil {
						err = fmt.Errorf("interface address: %w", e)
						break
					}
					family := "ipv6"
					if prefix.Addr().Is4() {
						family = "ipv4"
					}
					list = append(list, map[string]any{"family": family, "address": prefix.Addr().String(), "prefix_length": prefix.Bits()})
				}
			}
			// The addresses keep the kernel's order: an address added later,
			// such as a keepalived virtual address, comes after the
			// interface's own, and the default address is the first.
			if err != nil {
				s.Unavailable["/node/network/interfaces/"+pointerPart(link.Name)+"/addresses"] = err.Error()
			} else {
				item["addresses"] = list
			}
		}
		s.put("network.interfaces", values, nil)
		routes, err := defaultRoutes(links)
		s.put("network.default_routes", routes, err)
		for _, family := range []string{"ipv4", "ipv6"} {
			address, addressErr := "", err
			if addressErr == nil {
				address, addressErr = defaultAddress(family, routes, values)
			}
			s.put("network.default_"+family+"_address", address, addressErr)
		}
	}
	return s
}

func pointerPart(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
func (s *Snapshot) put(path string, v any, err error) {
	parts := strings.Split(path, ".")
	into := s.Values
	for _, part := range parts[:len(parts)-1] {
		if into[part] == nil {
			into[part] = map[string]any{}
		}
		into = into[part].(map[string]any)
	}
	if err != nil {
		s.Unavailable["/node/"+strings.Join(parts, "/")] = err.Error()
		return
	}
	into[parts[len(parts)-1]] = v
}
