package facts

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"syscall"

	"golang.org/x/sys/unix"
)

func defaultRoutes(links []net.Interface) ([]any, error) {
	data, err := syscall.NetlinkRIB(unix.RTM_GETROUTE, unix.AF_UNSPEC)
	if err != nil {
		return nil, err
	}
	return parseRoutes(data, links)
}

func parseRoutes(data []byte, links []net.Interface) ([]any, error) {
	messages, err := syscall.ParseNetlinkMessage(data)
	if err != nil {
		return nil, err
	}
	// ParseNetlinkMessage ignores a trailing fragment shorter than a header.
	// Do not turn a truncated observation into an apparently empty route list.
	consumed := 0
	for _, message := range messages {
		consumed += (int(message.Header.Len) + 3) &^ 3
	}
	if consumed != len(data) {
		return nil, fmt.Errorf("incomplete route response from the kernel")
	}
	names := map[int]string{}
	for _, link := range links {
		names[link.Index] = link.Name
	}
	out := []any{}
	for _, message := range messages {
		if message.Header.Flags&unix.NLM_F_DUMP_INTR != 0 {
			return nil, fmt.Errorf("route table changed during inspection")
		}
		if message.Header.Type == unix.NLMSG_ERROR {
			return nil, fmt.Errorf("route inspection failed")
		}
		if message.Header.Type != unix.RTM_NEWROUTE {
			continue
		}
		b := message.Data
		if len(b) < unix.SizeofRtMsg {
			return nil, fmt.Errorf("incomplete route record from the kernel")
		}
		family := ""
		switch b[0] {
		case unix.AF_INET:
			family = "ipv4"
		case unix.AF_INET6:
			family = "ipv6"
		default:
			continue
		}
		if b[1] != 0 || b[2] != 0 || b[7] != unix.RTN_UNICAST || binary.NativeEndian.Uint32(b[8:12])&unix.RTM_F_CLONED != 0 {
			continue
		}
		attrs, err := routeAttrs(b[unix.SizeofRtMsg:])
		if err != nil {
			return nil, err
		}
		table := uint32(b[4])
		if raw, ok := attrs[unix.RTA_TABLE]; ok {
			if len(raw) != 4 {
				return nil, fmt.Errorf("invalid route table")
			}
			table = binary.NativeEndian.Uint32(raw)
		}
		if table != unix.RT_TABLE_MAIN {
			continue
		}
		metric := uint32(0)
		if raw, ok := attrs[unix.RTA_PRIORITY]; ok {
			if len(raw) != 4 {
				return nil, fmt.Errorf("invalid route metric")
			}
			metric = binary.NativeEndian.Uint32(raw)
		}
		// The source address belongs to the route, also to each hop of a
		// route with several.
		source := attrs[unix.RTA_PREFSRC]
		add := func(index int, attrs map[uint16][]byte) error {
			name, ok := names[index]
			if !ok {
				return fmt.Errorf("default route refers to unavailable interface %d", index)
			}
			item := map[string]any{"family": family, "interface": name, "metric": int64(metric)}
			gateway := attrs[unix.RTA_GATEWAY]
			af := int(b[0])
			if via, ok := attrs[unix.RTA_VIA]; ok {
				if len(via) < 2 {
					return fmt.Errorf("invalid route gateway")
				}
				af = int(binary.NativeEndian.Uint16(via))
				gateway = via[2:]
			}
			if gateway != nil {
				addr, ok := netip.AddrFromSlice(gateway)
				if !ok || (af == unix.AF_INET && !addr.Is4()) || (af == unix.AF_INET6 && !addr.Is6()) || (af != unix.AF_INET && af != unix.AF_INET6) {
					return fmt.Errorf("invalid route gateway")
				}
				item["gateway"] = addr.String()
			}
			if source != nil {
				addr, ok := netip.AddrFromSlice(source)
				if !ok || (family == "ipv4") != addr.Is4() {
					return fmt.Errorf("invalid route source address")
				}
				item["source_address"] = addr.String()
			}
			out = append(out, item)
			return nil
		}
		if hops, ok := attrs[unix.RTA_MULTIPATH]; ok {
			for len(hops) > 0 {
				if len(hops) < unix.SizeofRtNexthop {
					return nil, fmt.Errorf("incomplete next-hop record in the kernel route response")
				}
				size := int(binary.NativeEndian.Uint16(hops))
				if size < unix.SizeofRtNexthop || size > len(hops) {
					return nil, fmt.Errorf("invalid next-hop record in the kernel route response")
				}
				nested, err := routeAttrs(hops[unix.SizeofRtNexthop:size])
				if err != nil {
					return nil, err
				}
				if err := add(int(binary.NativeEndian.Uint32(hops[4:8])), nested); err != nil {
					return nil, err
				}
				advance := (size + 3) &^ 3
				if advance > len(hops) {
					return nil, fmt.Errorf("incomplete padding after a next-hop record in the kernel route response")
				}
				hops = hops[advance:]
			}
		} else {
			raw, ok := attrs[unix.RTA_OIF]
			if !ok {
				return nil, fmt.Errorf("default route has no output interface")
			}
			if len(raw) != 4 {
				return nil, fmt.Errorf("invalid route interface")
			}
			if err := add(int(binary.NativeEndian.Uint32(raw)), attrs); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].(map[string]any), out[j].(map[string]any)
		if a["family"] != b["family"] {
			return a["family"].(string) < b["family"].(string)
		}
		if a["metric"] != b["metric"] {
			return a["metric"].(int64) < b["metric"].(int64)
		}
		if a["interface"] != b["interface"] {
			return a["interface"].(string) < b["interface"].(string)
		}
		ag, _ := a["gateway"].(string)
		bg, _ := b["gateway"].(string)
		return ag < bg
	})
	return out, nil
}
func routeAttrs(data []byte) (map[uint16][]byte, error) {
	out := map[uint16][]byte{}
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, fmt.Errorf("incomplete route attribute in the kernel response")
		}
		size := int(binary.NativeEndian.Uint16(data))
		if size < 4 || size > len(data) {
			return nil, fmt.Errorf("invalid route attribute")
		}
		kind := binary.NativeEndian.Uint16(data[2:4]) & 0x3fff // Strip nested/network-byte-order flags.
		out[kind] = data[4:size]
		advance := (size + 3) &^ 3
		if advance > len(data) {
			return nil, fmt.Errorf("incomplete padding after a route attribute in the kernel response")
		}
		data = data[advance:]
	}
	return out, nil
}
