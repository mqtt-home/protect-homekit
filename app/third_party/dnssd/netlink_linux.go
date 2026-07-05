package dnssd

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/brutella/dnssd/log"
	"github.com/vishvananda/netlink"
)

// linkUpdateDebounce coalesces bursts of netlink events (interface
// flaps, container churn) into a single re-announce decision.
const linkUpdateDebounce = 3 * time.Second

// linkSubscribe watches netlink for interface and address changes.
// Every event invalidates the shared interface snapshot (ifcache.go).
// After a debounce window, all services are re-announced — but only if
// the set of served interfaces or their addresses actually changed,
// so irrelevant events (wifi state, veth churn without address moves,
// repeated carrier flaps that settle in the same state) stay quiet.
func (r *responder) linkSubscribe(ctx context.Context) {
	done := make(chan struct{})
	defer close(done)

	linkCh := make(chan netlink.LinkUpdate, 16)
	if err := netlink.LinkSubscribe(linkCh, done); err != nil {
		log.Info.Println("dnssd: link subscribe:", err)
		return
	}

	addrCh := make(chan netlink.AddrUpdate, 16)
	if err := netlink.AddrSubscribe(addrCh, done); err != nil {
		log.Info.Println("dnssd: addr subscribe:", err)
		addrCh = nil
	}

	// With event-driven invalidation active, the snapshot TTL is only
	// a safety net.
	extendIfaceSnapshotTTL()

	log.Debug.Println("waiting for link updates...")

	var pending <-chan time.Time
	last := r.servedIfacesFingerprint()

	for {
		select {
		case _, ok := <-linkCh:
			if !ok {
				linkCh = nil
				continue
			}
			invalidateIfaceSnapshot()
			if pending == nil {
				pending = time.After(linkUpdateDebounce)
			}
		case _, ok := <-addrCh:
			if !ok {
				addrCh = nil
				continue
			}
			invalidateIfaceSnapshot()
			if pending == nil {
				pending = time.After(linkUpdateDebounce)
			}
		case <-pending:
			pending = nil

			fp := r.servedIfacesFingerprint()
			if fp == last {
				log.Debug.Println("link update: no relevant interface change")
				continue
			}
			last = fp

			log.Debug.Println("announcing services after link update")
			r.mutex.Lock()
			r.announce(services(r.managed))
			r.mutex.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// servedIfacesFingerprint captures the state of the interfaces the
// responder's services are published on: name, index, up/running
// flags and IP addresses. Announcements are only sent when this
// changes.
func (r *responder) servedIfacesFingerprint() string {
	r.mutex.Lock()
	srvs := services(r.managed)
	r.mutex.Unlock()

	snap := getIfaceSnapshot()

	seen := map[string]bool{}
	entries := []string{}
	for _, srv := range srvs {
		for _, iface := range srv.Interfaces() {
			if seen[iface.Name] {
				continue
			}
			seen[iface.Name] = true

			ips := []string{}
			for _, ip := range snap.ips[iface.Name] {
				ips = append(ips, ip.String())
			}
			sort.Strings(ips)

			flags := iface.Flags & (net.FlagUp | net.FlagRunning)
			entries = append(entries, fmt.Sprintf("%s|%d|%s|%s", iface.Name, iface.Index, flags, strings.Join(ips, ",")))
		}
	}
	sort.Strings(entries)

	return strings.Join(entries, ";")
}
