package facts

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// distributionFamilies maps an os-release ID to the family whose tools the
// distribution uses. An ID that is not listed takes the family of the first
// ID_LIKE word that is.
var distributionFamilies = map[string]string{
	"debian": "debian", "ubuntu": "debian",
	"rhel": "redhat", "centos": "redhat", "fedora": "redhat", "rocky": "redhat", "almalinux": "redhat", "amzn": "redhat", "ol": "redhat",
	"arch":     "arch",
	"opensuse": "suse", "opensuse-leap": "suse", "opensuse-tumbleweed": "suse", "sles": "suse", "suse": "suse",
}

// packageManagers names the program that installs packages on each family.
var packageManagers = map[string]string{"debian": "apt", "redhat": "dnf", "arch": "pacman", "suse": "zypper"}

// gatherDistribution derives the distribution facts from os-release. A field
// the distribution does not set is unavailable with its reason, never empty.
func gatherDistribution(s *Snapshot, release map[string]any, err error) {
	field := func(key string) string { text, _ := release[key].(string); return text }
	id := field("id")
	if err == nil && id == "" {
		err = errors.New("os-release has no ID")
	}
	s.put("distribution.id", id, err)
	version, versionErr := field("version_id"), err
	if versionErr == nil && version == "" {
		versionErr = errors.New("os-release has no VERSION_ID; a rolling release has none")
	}
	s.put("distribution.version", version, versionErr)
	major := 0
	if versionErr == nil {
		digits, _, _ := strings.Cut(version, ".")
		if major, versionErr = strconv.Atoi(digits); versionErr != nil {
			versionErr = fmt.Errorf("VERSION_ID %q does not start with a number", version)
		}
	}
	s.put("distribution.major_version", major, versionErr)
	codename, codenameErr := field("version_codename"), err
	if codenameErr == nil && codename == "" {
		codenameErr = errors.New("os-release has no VERSION_CODENAME")
	}
	s.put("distribution.codename", codename, codenameErr)
	family, familyErr := distributionFamilies[id], err
	if familyErr == nil && family == "" {
		for _, like := range strings.Fields(field("id_like")) {
			if family = distributionFamilies[like]; family != "" {
				break
			}
		}
		if family == "" {
			familyErr = fmt.Errorf("MMX does not know the family of distribution %q", id)
		}
	}
	s.put("distribution.family", family, familyErr)
	manager, managerErr := packageManagers[family], familyErr
	if managerErr == nil && manager == "" {
		managerErr = fmt.Errorf("MMX does not know the package manager of the %s family", family)
	}
	s.put("package_manager", manager, managerErr)
}

// virtualizationType runs systemd-detect-virt, which prints "none" and exits
// with 1 on physical hardware.
func virtualizationType() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemd-detect-virt").Output()
	text := strings.TrimSpace(string(out))
	if text == "" {
		if err == nil {
			err = errors.New("systemd-detect-virt printed nothing")
		}
		return "", fmt.Errorf("systemd-detect-virt: %w", err)
	}
	return text, nil
}

// fullyQualifiedName is the canonical name the node's resolver gives its own
// hostname, from /etc/hosts or DNS; a node without a domain keeps its hostname.
func fullyQualifiedName(hostname string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	name, err := net.DefaultResolver.LookupCNAME(ctx, hostname)
	if name = strings.TrimSuffix(name, "."); err != nil || name == "" {
		return hostname
	}
	return name
}

// defaultAddress is the first address of a family on the interface that
// carries that family's default route with the lowest metric. An IPv6
// link-local address never counts.
func defaultAddress(family string, routes []any, interfaces map[string]any) (string, error) {
	var route map[string]any
	for _, item := range routes {
		r := item.(map[string]any)
		if r["family"] == family && (route == nil || r["metric"].(int64) < route["metric"].(int64)) {
			route = r
		}
	}
	if route == nil {
		return "", fmt.Errorf("the node has no %s default route", family)
	}
	// The kernel sends the route's packets from its source address, such
	// as the one a DHCP client sets.
	if source, ok := route["source_address"].(string); ok {
		return source, nil
	}
	name := route["interface"].(string)
	link, _ := interfaces[name].(map[string]any)
	addresses, ok := link["addresses"].([]any)
	if !ok {
		return "", fmt.Errorf("the addresses of interface %s are unavailable", name)
	}
	for _, item := range addresses {
		a := item.(map[string]any)
		ip := net.ParseIP(a["address"].(string))
		if a["family"] == family && !ip.IsLinkLocalUnicast() {
			return a["address"].(string), nil
		}
	}
	return "", fmt.Errorf("interface %s has no %s address", name, family)
}
