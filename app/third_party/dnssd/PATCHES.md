# Local patches on top of github.com/brutella/dnssd v1.2.14

This is a vendored copy of `github.com/brutella/dnssd@v1.2.14`, wired in via a
`replace` directive in the parent module's `go.mod`. It carries local patches
to cut CPU usage on always-on, low-power hosts (Raspberry Pi).

## Why

CPU and allocs profiles showed the process dominated by `syscall.NetlinkRIB`:
on Linux, `net.Interfaces`, `net.InterfaceByIndex/-Name` and `iface.Addrs()`
each dump the kernel's **entire** interface (or address) table via netlink —
even when asking for a single interface — and upstream dnssd calls them on
the per-packet read path (mDNS is constant background traffic on a home
network). An earlier iteration of these patches added separate 60s TTL caches
per lookup site, but with N interfaces that still cost roughly N full-table
dumps per minute per cache (each dump proportional to N), plus a full
re-announce of all services on every netlink link flap.

## Patches

1. **ifcache.go** (new file) — a single shared snapshot of the interface
   table (interfaces by index/name, per-interface IPs/networks, multicast
   list) built with one `net.Interfaces()` pass. All lookups are served from
   it: `interfaceByIndexCached()` / `interfaceByNameCached()` (read loops,
   `Service.Interfaces`), `ipsAtInterfaceCached()` (record building,
   announcements), `MulticastInterfaces()` (exported API; filters applied
   in-memory) and `getInterfaceByIp()` (mdns.go, non-control-message
   fallback). The snapshot is rebuilt when:
   - it is older than its TTL — 60s by default, raised to 15min on Linux
     once the netlink event subscription is active (the TTL is then only a
     safety net);
   - a netlink link/addr event invalidates it (`invalidateIfaceSnapshot()`);
   - a by-index/by-name lookup misses and the snapshot is older than 5s
     (`ifaceSnapMissRefresh`), so a brand-new interface is picked up quickly
     but unknown/bogus interface indexes cannot force per-packet dumps
     (upstream re-dumped the table for every packet whose lookup failed).

2. **netlink_linux.go** — `linkSubscribe()` rewritten: subscribes to both
   link *and* address updates, invalidates the snapshot on every event, and
   debounces bursts (3s). After the debounce it re-announces services **only
   when the fingerprint of served interfaces actually changed** (name, index,
   up/running flags, addresses). Upstream re-announced all services on every
   link update — on hosts with container/veth churn that meant constant
   multicast announcement bursts waking every mDNS device on the LAN.

3. **responder.go** — `containsConflictingAnswers()`: filter the request's
   records for this service's hostname *first* and return early when there
   are none, so our own A/AAAA records are only built for the rare packets
   that actually mention this service.

4. **cache.go / probe.go** — same-host service-instance conflict detection.
   Upstream `filterRecords()` treats *any* SRV whose target equals our own
   hostname as "ourself" and drops it, so a second registrant on the same
   host claiming the same service instance name (but a different port) is
   never seen as a conflict and both keep the name. The filter now only
   drops an SRV when target **and** port match ours (truly identical rdata,
   which is genuinely not a conflict per RFC 6762). `probe.go` additionally
   keeps a detected `serviceName` conflict sticky within a probe cycle
   (matching upstream's existing handling of hostname conflicts), so a later
   response without SRV records can't clear it before the rename fires.

Interface/address changes are picked up via netlink events (Linux) or after
at most 60s (other platforms, snapshot TTL), both well below mDNS record
TTLs.

Also removed: the upstream `cmd/` directory (example binaries, not needed).

## Upgrading

Check whether upstream (https://github.com/brutella/dnssd) has fixed the
per-packet interface lookups. If yes, drop this directory and the `replace`
directive. If no, re-copy the new version and re-apply the patches above
(ifcache.go can be copied as-is; mdns.go/service.go need their local caches
and direct `net.*` interface lookups replaced with the snapshot calls).
