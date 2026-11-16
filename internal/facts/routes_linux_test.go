package facts

import (
	"encoding/binary"
	"net"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func u32(n uint32) []byte { b := make([]byte, 4); binary.NativeEndian.PutUint32(b, n); return b }
func attr(kind uint16, data []byte) []byte {
	b := make([]byte, (len(data)+7)&^3)
	binary.NativeEndian.PutUint16(b, uint16(4+len(data)))
	binary.NativeEndian.PutUint16(b[2:], kind)
	copy(b[4:], data)
	return b
}
func route(af, table, prefix byte, attrs ...[]byte) []byte {
	body := make([]byte, unix.SizeofRtMsg)
	body[0], body[1], body[4], body[7] = af, prefix, table, unix.RTN_UNICAST
	for _, a := range attrs {
		body = append(body, a...)
	}
	b := make([]byte, unix.NLMSG_HDRLEN+len(body))
	binary.NativeEndian.PutUint32(b, uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:], unix.RTM_NEWROUTE)
	copy(b[unix.NLMSG_HDRLEN:], body)
	return b
}
func hop(index uint32, gateway []byte) []byte {
	a := attr(unix.RTA_GATEWAY, gateway)
	b := make([]byte, 8)
	binary.NativeEndian.PutUint16(b, uint16(8+len(a)))
	binary.NativeEndian.PutUint32(b[4:], index)
	return append(b, a...)
}
func TestMainTableDefaultsIncludeFamiliesMetricsAndMultipath(t *testing.T) {
	var wire []byte
	for _, r := range [][]byte{
		route(unix.AF_INET, unix.RT_TABLE_MAIN, 0, attr(unix.RTA_OIF, u32(2)), attr(unix.RTA_GATEWAY, []byte{10, 0, 0, 1}), attr(unix.RTA_PRIORITY, u32(100)), attr(unix.RTA_PREFSRC, []byte{10, 0, 0, 5})),
		route(unix.AF_INET, 0, 0, attr(unix.RTA_TABLE, u32(unix.RT_TABLE_MAIN)), attr(unix.RTA_MULTIPATH, append(hop(3, []byte{10, 1, 0, 1}), hop(2, []byte{10, 0, 0, 2})...))),
		route(unix.AF_INET6, unix.RT_TABLE_MAIN, 0, attr(unix.RTA_OIF, u32(2)), attr(unix.RTA_GATEWAY, net.ParseIP("fe80::1").To16())),
		route(unix.AF_INET, unix.RT_TABLE_MAIN, 0, attr(unix.RTA_OIF, u32(3)), attr(unix.RTA_PRIORITY, u32(200))),
		route(unix.AF_INET, unix.RT_TABLE_LOCAL, 0, attr(unix.RTA_OIF, u32(2))),
		route(unix.AF_INET, unix.RT_TABLE_MAIN, 24, attr(unix.RTA_OIF, u32(2))),
	} {
		wire = append(wire, r...)
	}
	got, err := parseRoutes(wire, []net.Interface{{Index: 2, Name: "eth0"}, {Index: 3, Name: "eth1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"family": "ipv4", "interface": "eth0", "gateway": "10.0.0.2", "metric": int64(0)},
		map[string]any{"family": "ipv4", "interface": "eth1", "gateway": "10.1.0.1", "metric": int64(0)},
		map[string]any{"family": "ipv4", "interface": "eth0", "gateway": "10.0.0.1", "metric": int64(100), "source_address": "10.0.0.5"},
		map[string]any{"family": "ipv4", "interface": "eth1", "metric": int64(200)},
		map[string]any{"family": "ipv6", "interface": "eth0", "gateway": "fe80::1", "metric": int64(0)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
	empty, err := parseRoutes(nil, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
	for _, bad := range [][]byte{wire[:10], route(unix.AF_INET, unix.RT_TABLE_MAIN, 0, attr(unix.RTA_OIF, []byte{1})), route(unix.AF_INET, unix.RT_TABLE_MAIN, 0, attr(unix.RTA_OIF, u32(999)))} {
		if _, err := parseRoutes(bad, nil); err == nil {
			t.Fatal("accepted invalid route")
		}
	}
}
