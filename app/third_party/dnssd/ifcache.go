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
//   - a netlink link/addr event marked it dirty AND the last rebuild was
//     more than ifaceSnapEventRefresh ago. On a busy host (e.g. a
//     hostNetwork pod that sees every container/veth event) events can
//     arrive faster than mDNS packets; without this rate limit each
//     event would force the next packet to redo a full kernel dump. The
//     stale snapshot keeps being served until the interval elapses, so
//     changes are still picked up within a few seconds.
//   - a by-index/by-name lookup misses (e.g. first packet from a
//     just-created interface) and the snapshot is older than
//     ifaceSnapMissRefresh. The rate limit keeps packets with bogus
//     interface indexes from forcing per-packet kernel dumps.
//
// The re-announce path (netlink_linux.go) uses getIfaceSnapshotFresh to
// bypass the rate limit; it is already debounced, so an accurate view
// matters more there than rebuild frequency.
const (
	ifaceSnapDefaultTTL   = 60 * time.Second
	ifaceSnapEventTTL     = 15 * time.Minute
	ifaceSnapMissRefresh  = 5 * time.Second
	ifaceSnapEventRefresh = 5 * time.Second
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
	dirty   bool // a netlink event arrived; rebuild is due (rate-limited)
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

// rebuildIfaceSnapshotLocked rebuilds the snapshot now and clears the
// dirty flag. ifaceSnap must be locked by the caller.
func rebuildIfaceSnapshotLocked() *ifaceSnapshot {
	ifaceSnap.snap = buildIfaceSnapshot()
	ifaceSnap.fetched = time.Now()
	ifaceSnap.dirty = false
	return ifaceSnap.snap
}

// ifaceSnapshotLocked returns the current snapshot for the per-packet
// path. It rebuilds when the snapshot is missing, when the TTL safety
// net has expired, or when a netlink event marked it dirty — but a
// dirty rebuild happens at most once per ifaceSnapEventRefresh, so a
// storm of events on a busy (e.g. hostNetwork) host cannot force a
// kernel interface dump per received mDNS packet. ifaceSnap must be
// locked by the caller.
func ifaceSnapshotLocked() *ifaceSnapshot {
	if ifaceSnap.snap == nil ||
		time.Since(ifaceSnap.fetched) >= ifaceSnap.ttl ||
		(ifaceSnap.dirty && time.Since(ifaceSnap.fetched) >= ifaceSnapEventRefresh) {
		return rebuildIfaceSnapshotLocked()
	}
	return ifaceSnap.snap
}

func getIfaceSnapshot() *ifaceSnapshot {
	ifaceSnap.Lock()
	defer ifaceSnap.Unlock()
	return ifaceSnapshotLocked()
}

// getIfaceSnapshotFresh forces a rebuild when the snapshot is dirty,
// bypassing the per-packet rate limit. Used by the re-announce path,
// which is already debounced (so it runs at most once per few seconds)
// and needs an accurate view to decide whether interfaces changed.
func getIfaceSnapshotFresh() *ifaceSnapshot {
	ifaceSnap.Lock()
	defer ifaceSnap.Unlock()
	if ifaceSnap.snap == nil || ifaceSnap.dirty {
		return rebuildIfaceSnapshotLocked()
	}
	return ifaceSnap.snap
}

// invalidateIfaceSnapshot marks the cached interface table stale so it
// is rebuilt on the next access (rate-limited on the per-packet path).
// The current snapshot keeps being served until then. Called on netlink
// link/addr update events.
func invalidateIfaceSnapshot() {
	ifaceSnap.Lock()
	defer ifaceSnap.Unlock()
	ifaceSnap.dirty = true
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
	return rebuildIfaceSnapshotLocked()
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
