package modules

import (
	"fmt"
	"strings"
)

// packageManagerNames lists the package managers in the order a message names
// them; they are the values of the node fact package_manager.
var packageManagerNames = []string{"apt", "dnf", "pacman", "zypper"}

// programPackages names, for each program that a tool module runs, the
// package that provides it under each package manager. A package manager
// that has no entry has no package for the program: the program does not
// exist on that family, or its distributions ship it outside their package
// repositories. One entry serves every module that runs the program.
var programPackages = map[string]map[string]string{
	"git":             allManagers("git", "git-core", "git", "git"),
	"ssh-keygen":      allManagers("openssh-client", "openssh", "openssh", "openssh-clients"),
	"getfacl":         allManagers("acl", "acl", "acl", "acl"),
	"setfacl":         allManagers("acl", "acl", "acl", "acl"),
	"getcap":          allManagers("libcap2-bin", "libcap", "libcap", "libcap-progs"),
	"setcap":          allManagers("libcap2-bin", "libcap", "libcap", "libcap-progs"),
	"modprobe":        allManagers("kmod", "kmod", "kmod", "kmod"),
	"hostnamectl":     allManagers("systemd", "systemd", "systemd", "systemd"),
	"timedatectl":     allManagers("systemd", "systemd", "systemd", "systemd"),
	"systemctl":       allManagers("systemd", "systemd", "systemd", "systemd"),
	"journalctl":      allManagers("systemd", "systemd", "systemd", "systemd"),
	"systemd-analyze": allManagers("systemd", "systemd", "systemd", "systemd"),
	"networkctl":      allManagers("systemd", "systemd-networkd", "systemd", "systemd-networkd"),
	// cronie installs crond, which the check for cron does not accept.
	"cron":                allManagers("cron", "cronie", "", "cron"),
	"ip":                  allManagers("iproute2", "iproute", "iproute2", "iproute2"),
	"wg":                  allManagers("wireguard-tools", "wireguard-tools", "wireguard-tools", "wireguard-tools"),
	"wg-quick":            allManagers("wireguard-tools", "wireguard-tools", "wireguard-tools", "wireguard-tools"),
	"ufw":                 allManagers("ufw", "", "ufw", "ufw"),
	"podman":              allManagers("podman", "podman", "podman", "podman"),
	"skopeo":              allManagers("skopeo", "skopeo", "skopeo", "skopeo"),
	"docker":              allManagers("docker.io or docker-ce", "docker-ce", "docker", "docker"),
	"certbot":             allManagers("certbot", "certbot, from the EPEL package repository", "certbot", "python313-certbot"),
	"psql":                allManagers("postgresql-client", "postgresql", "postgresql-libs", "postgresql"),
	"pg_isready":          allManagers("postgresql-client", "postgresql", "postgresql-libs", "postgresql"),
	"mariadb":             allManagers("mariadb-client or mysql-client", "mariadb or mysql", "mariadb-clients", "mariadb-client"),
	"mysql":               allManagers("mariadb-client or mysql-client", "mariadb or mysql", "mariadb-clients", "mariadb-client"),
	"my_print_defaults":   allManagers("mariadb-client or mysql-client", "mariadb or mysql", "mariadb", "mariadb"),
	"apt-get":             {"apt": "apt"},
	"update-alternatives": {"apt": "dpkg", "dnf": "alternatives"},
	"locale":              {"apt": "libc-bin", "dnf": "glibc-common"},
	"localectl":           {"dnf": "systemd"},
	"dnf":                 {"dnf": "dnf"},
	"firewall-cmd":        {"dnf": "firewalld", "zypper": "firewalld"},
	"gpg":                 {"dnf": "gnupg2"},
	"nmcli":               allManagers("network-manager", "NetworkManager", "networkmanager", "NetworkManager"),
	"semanage":            {"dnf": "policycoreutils-python-utils"},
	"setsebool":           {"dnf": "policycoreutils"},
	"restorecon":          {"dnf": "policycoreutils"},
	"matchpathcon":        {"dnf": "libselinux-utils"},
	"locale-gen":          {"apt": "locales"},
	"update-locale":       {"apt": "locales"},
	"netplan":             {"apt": "netplan.io"},
	"ifup":                {"apt": "ifupdown"},
	"ifdown":              {"apt": "ifupdown"},
	"ifquery":             {"apt": "ifupdown"},
	"snap":                {"apt": "snapd", "dnf": "snapd, from the EPEL package repository"},
	"python3":             allManagers("python3", "python3", "python", "python3"),
	"gem":                 allManagers("ruby", "rubygems", "ruby", "ruby"),
	"npm":                 allManagers("npm", "npm", "npm", "npm"),
	"uv":                  allManagers("uv, from its release archive", "uv, from its release archive", "uv", "python313-uv"),
	"wipefs":              util("util-linux"),
	"findmnt":             util("util-linux"),
	"findfs":              util("util-linux"),
	"mkswap":              util("util-linux"),
	"mount":               allManagers("mount", "util-linux", "util-linux", "util-linux"),
	"umount":              allManagers("mount", "util-linux", "util-linux", "util-linux"),
	"swapon":              allManagers("util-linux", "util-linux", "util-linux", "util-linux"),
	"swapoff":             allManagers("util-linux", "util-linux", "util-linux", "util-linux"),
	"mkfs.ext2":           allManagers("e2fsprogs", "e2fsprogs", "e2fsprogs", "e2fsprogs"),
	"mkfs.ext3":           allManagers("e2fsprogs", "e2fsprogs", "e2fsprogs", "e2fsprogs"),
	"mkfs.ext4":           allManagers("e2fsprogs", "e2fsprogs", "e2fsprogs", "e2fsprogs"),
	"mkfs.xfs":            allManagers("xfsprogs", "xfsprogs", "xfsprogs", "xfsprogs"),
	"mkfs.btrfs":          allManagers("btrfs-progs", "btrfs-progs", "btrfs-progs", "btrfs-progs"),
	"mkfs.vfat":           allManagers("dosfstools", "dosfstools", "dosfstools", "dosfstools"),
	"mkfs.exfat":          allManagers("exfatprogs", "exfatprogs", "exfatprogs", "exfatprogs"),
	"mkfs.f2fs":           allManagers("f2fs-tools", "f2fs-tools", "f2fs-tools", "f2fs-tools"),
}

// allManagers is an entry for apt, dnf, pacman, and zypper in that order; ""
// leaves the package manager out.
func allManagers(apt, dnf, pacman, zypper string) map[string]string {
	entry := map[string]string{}
	for manager, name := range map[string]string{"apt": apt, "dnf": dnf, "pacman": pacman, "zypper": zypper} {
		if name != "" {
			entry[manager] = name
		}
	}
	return entry
}

func util(name string) map[string]string { return allManagers(name, name, name, name) }

// missingProgramError says that a program a module needs is not installed,
// and what provides the program under each package manager that has it. A
// program that the table does not know is named alone.
func missingProgramError(program string) error {
	names := programPackages[program]
	if len(names) == 0 {
		return fmt.Errorf("%s is not installed; install the package that provides it in an earlier node task", program)
	}
	managersOfPackage := map[string][]string{}
	var packages []string
	for _, manager := range packageManagerNames {
		if name, ok := names[manager]; ok {
			if _, seen := managersOfPackage[name]; !seen {
				packages = append(packages, name)
			}
			managersOfPackage[name] = append(managersOfPackage[name], manager)
		}
	}
	if len(packages) == 1 && len(managersOfPackage[packages[0]]) == len(names) && len(names) == len(packageManagerNames) {
		return fmt.Errorf("%s is not installed; install the package %s in an earlier node task", program, packages[0])
	}
	parts := make([]string, 0, len(packages))
	for _, name := range packages {
		parts = append(parts, fmt.Sprintf("%s (%s)", name, strings.Join(managersOfPackage[name], ", ")))
	}
	return fmt.Errorf("%s is not installed; install the package that provides it in an earlier node task: %s", program, strings.Join(parts, ", "))
}

// programNotFound is the error of a command whose program is not on the
// node: the message names the program and what provides it, and the cause
// stays visible to errors.Is, such as exec.ErrNotFound.
type programNotFound struct {
	name  string
	cause error
}

func (p programNotFound) Error() string { return missingProgramError(p.name).Error() }
func (p programNotFound) Unwrap() error { return p.cause }
