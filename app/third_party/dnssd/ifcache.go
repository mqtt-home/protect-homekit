package dnssd

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// On Linux, every net.Interfaces, net.InterfaceByIndex/-Name and
// iface.Addrs() call dumps the kernel's entire interface (or address)
// table via netlink — even when asking for a single interface. dnssd
// needs these lookups on the per-packet read path, so instead of
// hitting the kernel each time, all lookups are served from a single
// shared snapshot of the interface table.
//
// The snapshot is rebuilt when:
//   - it is older than its TTL (60s by default; raised on Linux once
//     the netlink event subscription is active, see netlink_linux.go),
//   - it has been invalidated by a netlink link/addr update event, or
//   - a by-index/by-name lookup misses (e.g. first packet from a
//     just-created interface) and the snapshot is older than
//     ifaceSnapMissRefresh. The rate limit keeps packets with bogus
//     interface indexes from forcing per-packet kernel dumps.
const (
	ifaceSnapDefaultTTL  = 60 * time.Second
	ifaceSnapEventTTL    = 15 * time.Minute
	ifaceSnapMissRefresh = 5 * time.Second
)

type ifaceSnapshot struct {
	ifaces    []net.Interface
	byIndex   map[int]*net.Interface
	byName    map[string]*net.Interface
	ips       map[string][]net.IP     // by name; up interfaces only
	nets      map[string][]*net.IPNet // by name; up interfaces only
	multicast []*net.Interface        // up multicast interfaces with at least one IP
}

var ifaceSnap = struct {
	sync.Mutex
	snap    *ifaceSnapshot
	fetched time.Time
	ttl     time.Duration
}{
	ttl: ifaceSnapDefaultTTL,
}

func buildIfaceSnapshot() *ifaceSnapshot {
	snap := &ifaceSnapshot{
		byIndex: map[int]*net.Interface{},
		byName:  map[string]*net.Interface{},
		ips:     map[string][]net.IP{},
		nets:    map[string][]*net.IPNet{},
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return snap
	}

	snap.ifaces = ifaces
	for i := range snap.ifaces {
		iface := &snap.ifaces[i]
		snap.byIndex[iface.Index] = iface
		snap.byName[iface.Name] = iface

		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		ips := []net.IP{}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				ips = append(ips, ipnet.IP)
				snap.nets[iface.Name] = append(snap.nets[iface.Name], ipnet)
			}
		}
		snap.ips[iface.Name] = ips

		if iface.Flags&net.FlagMulticast != 0 && len(ips) > 0 {
			snap.multicast = append(snap.multicast, iface)
		}
	}

	return snap
}

// ifaceSnapshotLocked returns the current snapshot, rebuilding it if it
// is missing or expired. ifaceSnap must be locked by the caller.
func ifaceSnapshotLocked() *ifaceSnapshot {
	if ifaceSnap.snap == nil || time.Since(ifaceSnap.fetched) >= ifaceSnap.ttl {
		ifaceSnap.snap = buildIfaceSnapshot()
		ifaceSnap.fetched = time.Now()
	}
	return ifaceSnap.snap
}

func getIfaceSnapshot() *ifaceSnapshot {
	ifaceSnap.Lock()
	defer ifaceSnap.Unlock()
	return ifaceSnapshotLocked()
}

// invalidateIfaceSnapshot drops the cached interface table so the next
// lookup rebuilds it. Called on netlink link/addr update events.
func invalidateIfaceSnapshot() {
	ifaceSnap.Lock()
	defer ifaceSnap.Unlock()
	ifaceSnap.snap = nil
}

// extendIfaceSnapshotTTL raises the snapshot TTL once event-driven
// invalidation is active; the TTL is then only a safety net.
func extendIfaceSnapshotTTL() {
	ifaceSnap.Lock()
	defer ifaceSnap.Unlock()
	ifaceSnap.ttl = ifaceSnapEventTTL
}

// refreshOnMissLocked rebuilds the snapshot after a lookup miss, at
// most once per ifaceSnapMissRefresh. ifaceSnap must be locked.
func refreshOnMissLocked() *ifaceSnapshot {
	if time.Since(ifaceSnap.fetched) < ifaceSnapMissRefresh {
		return nil
	}
	ifaceSnap.snap = buildIfaceSnapshot()
	ifaceSnap.fetched = time.Now()
	return ifaceSnap.snap
}

func interfaceByIndexCached(index int) (*net.Interface, error) {
	ifaceSnap.Lock()
	defer ifaceSnap.Unlock()

	if iface, ok := ifaceSnapshotLocked().byIndex[index]; ok {
		return iface, nil
	}
	if snap := refreshOnMissLocked(); snap != nil {
		if iface, ok := snap.byIndex[index]; ok {
			return iface, nil
		}
	}

	return nil, fmt.Errorf("dnssd: no network interface with index %d", index)
}

func interfaceByNameCached(name string) (*net.Interface, error) {
	ifaceSnap.Lock()
	defer ifaceSnap.Unlock()

	if iface, ok := ifaceSnapshotLocked().byName[name]; ok {
		return iface, nil
	}
	if snap := refreshOnMissLocked(); snap != nil {
		if iface, ok := snap.byName[name]; ok {
			return iface, nil
		}
	}

	return nil, fmt.Errorf("dnssd: no network interface with name %s", name)
}

func ipsAtInterfaceCached(iface *net.Interface) []net.IP {
	if ips, ok := getIfaceSnapshot().ips[iface.Name]; ok {
		return ips
	}

	return []net.IP{}
}

// MulticastInterfaces returns a list of all active multicast network interfaces.
func MulticastInterfaces(filters ...string) []*net.Interface {
	snap := getIfaceSnapshot()

	ifaces := []*net.Interface{}
	for _, iface := range snap.multicast {
		if containsIfaces(iface.Name, filters) {
			ifaces = append(ifaces, iface)
		}
	}

	return ifaces
}
