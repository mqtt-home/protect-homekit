# Local patches on top of github.com/brutella/dnssd v1.2.14

This is a vendored copy of `github.com/brutella/dnssd@v1.2.14`, wired in via a
`replace` directive in the parent module's `go.mod`. It carries local patches
to cut CPU usage on always-on, low-power hosts (Raspberry Pi).

## Why

A CPU profile showed ~60% of the process CPU in `syscall.NetlinkRIB`: on
Linux, `net.InterfaceByIndex` and `iface.Addrs()` dump the kernel's entire
interface table via netlink on every call, and upstream dnssd calls them for
**every mDNS packet received** on the LAN (mDNS is constant background
traffic on a home network).

## Patches

1. **mdns.go** — `interfaceByIndexCached()`: caches `net.InterfaceByIndex`
   results for 60s (`ifaceCacheTTL`). Used by the per-packet read loops.
2. **responder.go** — `containsConflictingAnswers()`: filter the request's
   records for this service's hostname *first* and return early when there
   are none, so our own A/AAAA records (which require a kernel interface
   address lookup) are only built for the rare packets that actually mention
   this service.
3. **service.go** — `ipsAtInterfaceCached()`: caches `iface.Addrs()` results
   per interface name for 60s.
4. **service.go** — `MulticastInterfaces()`: caches the enumerated interface
   list for 60s. It runs on the per-packet path via `Service.Interfaces()` /
   `HasIPOnAnyInterface()` (`filterRecords` calls it for A/AAAA records that
   match our own hostname — including our own looped-back announcements,
   since dnssd enables multicast loopback) and did a `net.Interfaces()` dump
   plus one `iface.Addrs()` dump per interface on every call.

Interface/address changes are picked up after at most 60s, which is well
below mDNS record TTLs; dnssd additionally re-announces on netlink link
updates (`netlink_linux.go`).

Also removed: the upstream `cmd/` directory (example binaries, not needed).

## Upgrading

Check whether upstream (https://github.com/brutella/dnssd) has fixed the
per-packet interface lookups. If yes, drop this directory and the `replace`
directive. If no, re-copy the new version and re-apply the three patches
above.
