// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package magicsock

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/health"
	"tailscale.com/net/netcheck"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest"
	"tailscale.com/types/key"
	"tailscale.com/util/eventbus"
	"tailscale.com/util/eventbus/eventbustest"
)

func TestAcquireDERPRegionWaitsForServerInfo(t *testing.T) {
	derpMap, cleanupDERP := runDERPAndStun(t, t.Logf, localhostListener{}, netip.MustParseAddr("127.0.0.1"))
	defer cleanupDERP()

	stack := newMagicStack(t, t.Logf, localhostListener{}, derpMap)
	defer stack.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := stack.conn.AcquireDERPRegion(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if !lease.Ready() {
		t.Fatal("acquired DERP lease is not ready after ServerInfo")
	}
	if got := lease.RegionID(); got != 1 {
		t.Fatalf("lease region = %d, want 1", got)
	}
	if got := lease.Generation(); got == 0 {
		t.Fatal("lease has zero receive generation")
	}

	stack.conn.mu.Lock()
	dc := stack.conn.activeDerp[1].c
	stack.conn.mu.Unlock()
	if err := dc.Close(); err != nil {
		t.Fatal(err)
	}
	if lease.Ready() {
		t.Fatal("lease stayed ready after the DERP client detached its connection")
	}
}

func TestAcquireDERPRegionCanceledDoesNotAcquireLease(t *testing.T) {
	derpMap, cleanupDERP := runDERPAndStun(t, t.Logf, localhostListener{}, netip.MustParseAddr("127.0.0.1"))
	defer cleanupDERP()
	stack := newMagicStack(t, t.Logf, localhostListener{}, derpMap)
	defer stack.Close()

	stack.conn.mu.Lock()
	before := stack.conn.activeDerp[1].leaseRefs
	stack.conn.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stack.conn.AcquireDERPRegion(ctx, 1); err == nil {
		t.Fatal("AcquireDERPRegion accepted an already-canceled context")
	}
	stack.conn.mu.Lock()
	after := stack.conn.activeDerp[1].leaseRefs
	stack.conn.mu.Unlock()
	if after != before {
		t.Fatalf("canceled acquire changed lease refs from %d to %d", before, after)
	}
}

func TestDERPReceiveLeaseZeroValueIsSafe(t *testing.T) {
	var lease DERPReceiveLease
	if lease.Ready() {
		t.Fatal("zero-value lease reports ready")
	}
	lease.Close()
}

func TestDERPReceiveLeaseProtectsIdleNonHomeConnection(t *testing.T) {
	c := newConn(t.Logf)
	c.mu.Lock()
	c.activeDerp = map[int]activeDerp{
		7: {
			readyGeneration: 1,
			leaseRefs:       1,
			readyChanged:    make(chan struct{}),
			lastWrite:       ptrTo(time.Now().Add(-2 * derpInactiveCleanupTime)),
		},
	}
	c.mu.Unlock()
	c.cleanStaleDerp()
	c.mu.Lock()
	_, stillActive := c.activeDerp[7]
	if c.derpCleanupTimer != nil {
		c.derpCleanupTimer.Stop()
	}
	c.mu.Unlock()
	if !stillActive {
		t.Fatal("idle reaper closed a non-home DERP connection held by a lease")
	}
}

func ptrTo[T any](v T) *T { return &v }

func CheckDERPHeuristicTimes(t *testing.T) {
	if netcheck.PreferredDERPFrameTime <= frameReceiveRecordRate {
		t.Errorf("PreferredDERPFrameTime too low; should be at least frameReceiveRecordRate")
	}
}

func TestSetNetworkMapWithDERPRoutePolicyAtomicPublication(t *testing.T) {
	c := newConn(t.Logf)
	peer := key.NewNode().Public()
	disco := key.NewDisco().Public()
	oldPeer := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: disco, HomeDERP: 900}).View()
	c.SetNetworkMap(tailcfg.NodeView{}, []tailcfg.NodeView{oldPeer})

	c.mu.Lock()
	c.derpRoute = map[key.NodePublic]derpRoute{
		peer: {regionID: 900},
	}
	c.mu.Unlock()

	newPeer := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: disco, HomeDERP: 901}).View()
	wantRoute := AuthoritativeDERPRoute{Peer: peer, RegionID: 901, Generation: 7}
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{newPeer}, []AuthoritativeDERPRoute{wantRoute}); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	gotPeer := c.peersByID[1]
	gotRoute, authoritative := c.authoritativeDERPRoutes[peer]
	_, learned := c.derpRoute[peer]
	c.mu.Unlock()

	if got := gotPeer.HomeDERP(); got != wantRoute.RegionID {
		t.Errorf("published peer HomeDERP = %d, want %d", got, wantRoute.RegionID)
	}
	if !authoritative || gotRoute != wantRoute {
		t.Errorf("published authoritative route = (%+v, %v), want (%+v, true)", gotRoute, authoritative, wantRoute)
	}
	if learned {
		t.Error("stale learned route survived authoritative policy publication")
	}
}

func TestAuthoritativeDERPRouteSuppressesStaleLearnedRoute(t *testing.T) {
	c := newConn(t.Logf)
	peer := key.NewNode().Public()
	peerView := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public(), HomeDERP: 901}).View()
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{peerView}, []AuthoritativeDERPRoute{{Peer: peer, RegionID: 901, Generation: 1}}); err != nil {
		t.Fatal(err)
	}

	// Simulate a stale entry that predates policy publication. Reads must still
	// honor the authoritative policy even if such an entry is present.
	c.mu.Lock()
	c.derpRoute = map[key.NodePublic]derpRoute{peer: {regionID: 900}}
	c.mu.Unlock()

	if got := c.fallbackDERPRegionForPeer(peer); got != 0 {
		t.Fatalf("fallback DERP region for authoritative peer = %d, want 0", got)
	}
}

func TestAuthoritativeDERPRouteSuppressesRelearn(t *testing.T) {
	c := newConn(t.Logf)
	peer := key.NewNode().Public()
	peerView := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public(), HomeDERP: 901}).View()
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{peerView}, []AuthoritativeDERPRoute{{Peer: peer, RegionID: 901, Generation: 1}}); err != nil {
		t.Fatal(err)
	}

	c.addDerpPeerRoute(peer, 900, nil)
	c.mu.Lock()
	_, learned := c.derpRoute[peer]
	c.mu.Unlock()
	if learned {
		t.Fatal("authoritative peer relearned a reverse DERP route")
	}
}

func TestAuthoritativeDERPBlockedRouteSuppressesAllDERPFallback(t *testing.T) {
	c := newConn(t.Logf)
	peer := key.NewNode().Public()
	peerView := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public()}).View()
	blocked := AuthoritativeDERPRoute{Peer: peer, Action: DERPRouteBlocked, Generation: 1}
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{peerView}, []AuthoritativeDERPRoute{blocked}); err != nil {
		t.Fatal(err)
	}

	// A stale learned entry must not provide a route after an explicit block.
	c.mu.Lock()
	c.derpRoute = map[key.NodePublic]derpRoute{peer: {regionID: 901}}
	if c.authoritativeDERPInboundAllowedLocked(peer) {
		c.mu.Unlock()
		t.Fatal("blocked policy accepted inbound DERP")
	}
	c.mu.Unlock()
	if got := c.fallbackDERPRegionForPeer(peer); got != 0 {
		t.Fatalf("blocked peer fallback DERP region = %d, want 0", got)
	}
}

func TestAuthoritativeDERPInboundAllowsCrossRegionAndDeniesBlockedPeers(t *testing.T) {
	c := newConn(t.Logf)
	allowedPeer := key.NewNode().Public()
	blockedPeer := key.NewNode().Public()
	peers := []tailcfg.NodeView{
		(&tailcfg.Node{ID: 1, Key: allowedPeer, DiscoKey: key.NewDisco().Public(), HomeDERP: 901}).View(),
		(&tailcfg.Node{ID: 2, Key: blockedPeer, DiscoKey: key.NewDisco().Public()}).View(),
	}
	routes := []AuthoritativeDERPRoute{
		{Peer: allowedPeer, RegionID: 901, Generation: 1},
		{Peer: blockedPeer, Action: DERPRouteBlocked, Generation: 1},
	}
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, peers, routes); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.authoritativeDERPInboundAllowedLocked(allowedPeer) {
		t.Fatal("authorized peer rejected inbound DERP")
	}
	for _, tc := range []struct {
		name string
		peer key.NodePublic
	}{
		{"blocked peer", blockedPeer},
		{"unknown peer", key.NewNode().Public()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c.authoritativeDERPInboundAllowedLocked(tc.peer) {
				t.Fatal("unauthorized inbound DERP accepted")
			}
		})
	}
}

func TestLegacySetNetworkMapCannotClearArmedPolicy(t *testing.T) {
	c := newConn(t.Logf)
	peer := key.NewNode().Public()
	armedPeer := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public()}).View()
	blocked := AuthoritativeDERPRoute{Peer: peer, Action: DERPRouteBlocked, Generation: 1}
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{armedPeer}, []AuthoritativeDERPRoute{blocked}); err != nil {
		t.Fatal(err)
	}

	// This is the old ambient API. It must be a no-op after arming, even
	// though the passed peer would otherwise have a usable home.
	legacyPeer := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public(), HomeDERP: 901}).View()
	c.SetNetworkMap(tailcfg.NodeView{}, []tailcfg.NodeView{legacyPeer})
	c.mu.Lock()
	gotPeer := c.peersByID[1]
	gotRoute, ok := c.authoritativeDERPRoutes[peer]
	armed := c.authoritativeDERPArmed
	c.mu.Unlock()
	if !armed || !ok || gotRoute != blocked {
		t.Fatalf("legacy call cleared armed policy: armed=%v route=(%+v,%v)", armed, gotRoute, ok)
	}
	if got := gotPeer.HomeDERP(); got != 0 {
		t.Fatalf("legacy call replaced armed peer map HomeDERP=%d, want 0", got)
	}
}

func TestAuthoritativeDERPPolicyRejectsGenerationRollback(t *testing.T) {
	c := newConn(t.Logf)
	peer := key.NewNode().Public()
	peerView := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public(), HomeDERP: 901}).View()
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{peerView}, []AuthoritativeDERPRoute{{Peer: peer, RegionID: 901, Generation: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{peerView}, []AuthoritativeDERPRoute{{Peer: peer, RegionID: 901, Generation: 1}}); err == nil {
		t.Fatal("stale authoritative generation unexpectedly replaced newer policy")
	}
	c.mu.Lock()
	got := c.authoritativeDERPRoutes[peer]
	c.mu.Unlock()
	if got.Generation != 2 {
		t.Fatalf("generation after rejected rollback = %d, want 2", got.Generation)
	}
}

func TestAuthoritativeDERPPolicyRejectsSameGenerationEquivocation(t *testing.T) {
	c := newConn(t.Logf)
	peer := key.NewNode().Public()
	via := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public(), HomeDERP: 901}).View()
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{via}, []AuthoritativeDERPRoute{{Peer: peer, RegionID: 901, Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	blocked := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public()}).View()
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{blocked}, []AuthoritativeDERPRoute{{Peer: peer, Action: DERPRouteBlocked, Generation: 1}}); err == nil {
		t.Fatal("same-generation route equivocation unexpectedly accepted")
	}
}

func TestAuthoritativeDERPPolicyRefusesIncrementalPeerMutation(t *testing.T) {
	c := newConn(t.Logf)
	peer := key.NewNode().Public()
	peerView := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public(), HomeDERP: 901}).View()
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{peerView}, []AuthoritativeDERPRoute{{Peer: peer, RegionID: 901, Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	c.RemovePeer(peerView.ID())
	c.UpsertPeer((&tailcfg.Node{ID: peerView.ID(), Key: peer, DiscoKey: key.NewDisco().Public(), HomeDERP: 902}).View())
	c.mu.Lock()
	gotPeer := c.peersByID[peerView.ID()]
	gotRoute := c.authoritativeDERPRoutes[peer]
	c.mu.Unlock()
	if gotPeer.HomeDERP() != 901 || gotRoute.RegionID != 901 || gotRoute.Generation != 1 {
		t.Fatalf("incremental mutation changed armed policy: peer=%d route=%+v", gotPeer.HomeDERP(), gotRoute)
	}
}

func TestAuthoritativeDERPPolicyDropsQueuedInboundAfterWithdrawal(t *testing.T) {
	c := newConn(t.Logf)
	peer := key.NewNode().Public()
	initialPeer := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public(), HomeDERP: 901}).View()
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{initialPeer}, []AuthoritativeDERPRoute{{Peer: peer, RegionID: 901, Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	token := c.authoritativeDERPPolicyGeneration
	c.mu.Unlock()

	// This is the exact state of a frame after runDerpReader authorized and
	// queued it, before connBind consumes it.
	released := false
	queued := derpReadResult{
		regionID:                      900,
		src:                           peer,
		n:                             1,
		authoritativePolicyGeneration: token,
		copyBuf: func(dst []byte) int {
			released = true
			dst[0] = 0
			return 1
		},
	}
	withdrawnPeer := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: key.NewDisco().Public()}).View()
	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{withdrawnPeer}, []AuthoritativeDERPRoute{{Peer: peer, Action: DERPRouteBlocked, Generation: 2}}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	if n, _ := c.processDERPReadResult(queued, buf); n != 0 {
		t.Fatalf("queued frame survived withdrawal: %d bytes", n)
	}
	if !released {
		t.Fatal("withdrawn queued frame did not release the DERP reader")
	}
}

func TestSetNetworkMapWithDERPRoutePolicyRejectsInvalidPolicyUnchanged(t *testing.T) {
	peer := key.NewNode().Public()
	disco := key.NewDisco().Public()
	initialPeer := (&tailcfg.Node{ID: 1, Key: peer, DiscoKey: disco, HomeDERP: 901}).View()
	initialRoute := AuthoritativeDERPRoute{Peer: peer, RegionID: 901, Generation: 1}

	tests := []struct {
		name   string
		peers  []tailcfg.NodeView
		routes []AuthoritativeDERPRoute
	}{
		{
			name:   "HomeDERP mismatch",
			peers:  []tailcfg.NodeView{(&tailcfg.Node{ID: 1, Key: peer, DiscoKey: disco, HomeDERP: 902}).View()},
			routes: []AuthoritativeDERPRoute{{Peer: peer, RegionID: 903, Generation: 2}},
		},
		{
			name:   "zero region",
			peers:  []tailcfg.NodeView{initialPeer},
			routes: []AuthoritativeDERPRoute{{Peer: peer, Generation: 2}},
		},
		{
			name:   "blocked route with region",
			peers:  []tailcfg.NodeView{initialPeer},
			routes: []AuthoritativeDERPRoute{{Peer: peer, Action: DERPRouteBlocked, RegionID: 901, Generation: 2}},
		},
		{
			name:   "zero generation",
			peers:  []tailcfg.NodeView{initialPeer},
			routes: []AuthoritativeDERPRoute{{Peer: peer, RegionID: 901}},
		},
		{
			name:   "missing peer",
			peers:  nil,
			routes: []AuthoritativeDERPRoute{{Peer: peer, RegionID: 901, Generation: 2}},
		},
		{
			name:  "duplicate peer",
			peers: []tailcfg.NodeView{initialPeer},
			routes: []AuthoritativeDERPRoute{
				{Peer: peer, RegionID: 901, Generation: 2},
				{Peer: peer, RegionID: 901, Generation: 3},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newConn(t.Logf)
			if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, []tailcfg.NodeView{initialPeer}, []AuthoritativeDERPRoute{initialRoute}); err != nil {
				t.Fatal(err)
			}

			if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, tt.peers, tt.routes); err == nil {
				t.Fatal("invalid policy unexpectedly succeeded")
			}

			c.mu.Lock()
			gotPeer := c.peersByID[1]
			gotRoute, authoritative := c.authoritativeDERPRoutes[peer]
			c.mu.Unlock()
			if got := gotPeer.HomeDERP(); got != initialPeer.HomeDERP() {
				t.Errorf("peer HomeDERP changed after rejected policy: got %d, want %d", got, initialPeer.HomeDERP())
			}
			if !authoritative || gotRoute != initialRoute {
				t.Errorf("policy changed after rejection: got (%+v, %v), want (%+v, true)", gotRoute, authoritative, initialRoute)
			}
		})
	}
}

func TestAuthoritativeDERPPolicyRequiresCompletePeerCoverage(t *testing.T) {
	c := newConn(t.Logf)
	authoritativePeer := key.NewNode().Public()
	otherPeer := key.NewNode().Public()
	peers := []tailcfg.NodeView{
		(&tailcfg.Node{ID: 1, Key: authoritativePeer, DiscoKey: key.NewDisco().Public(), HomeDERP: 901}).View(),
		(&tailcfg.Node{ID: 2, Key: otherPeer, DiscoKey: key.NewDisco().Public(), HomeDERP: 902}).View(),
	}
	c.SetNetworkMap(tailcfg.NodeView{}, peers)
	c.mu.Lock()
	c.derpRoute = map[key.NodePublic]derpRoute{
		authoritativePeer: {regionID: 800},
		otherPeer:         {regionID: 801},
	}
	c.mu.Unlock()

	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, peers, []AuthoritativeDERPRoute{{Peer: authoritativePeer, RegionID: 901, Generation: 1}}); err == nil {
		t.Fatal("partial authoritative policy unexpectedly succeeded")
	}
	if got := c.fallbackDERPRegionForPeer(authoritativePeer); got != 800 {
		t.Errorf("rejected policy changed authoritative peer fallback DERP region = %d, want 800", got)
	}
	if got := c.fallbackDERPRegionForPeer(otherPeer); got != 801 {
		t.Errorf("rejected policy changed other peer fallback DERP region = %d, want 801", got)
	}
}

func TestForceSetNearestDERP(t *testing.T) {
	derpMap := &tailcfg.DERPMap{
		Regions: map[int]*tailcfg.DERPRegion{
			7: {
				RegionID:   7,
				RegionCode: "test",
				Nodes: []*tailcfg.DERPNode{
					{
						Name:     "7a",
						RegionID: 7,
						HostName: "derp7.test.unused",
						IPv4:     "127.0.0.1",
						IPv6:     "none",
					},
				},
			},
		},
	}

	// Force the real control health check so we can verify force=true bypasses it.
	tstest.Replace(t, &checkControlHealthDuringNearestDERPInTests, true)

	bus := eventbustest.NewBus(t)
	ht := health.NewTracker(bus)
	c := newConn(t.Logf)
	ec := bus.Client("magicsock.Conn.Test")
	c.eventClient = ec
	c.homeDERPChangedPub = eventbus.Publish[HomeDERPChanged](ec)
	c.eventBus = bus
	c.derpMap = derpMap
	c.health = ht

	ht.SetOutOfPollNetMap()

	tw := eventbustest.NewWatcher(t, bus)

	got := c.ForceSetNearestDERP(7)
	if got != 7 {
		t.Fatalf("ForceSetNearestDERP(7) = %d, want 7", got)
	}
	if c.myDerp != 7 {
		t.Errorf("c.myDerp = %d after ForceSetNearestDERP, want 7", c.myDerp)
	}

	if err := eventbustest.Expect(tw, func(e HomeDERPChanged) error {
		if e.Old != 0 || e.New != 7 {
			return fmt.Errorf("got HomeDERPChanged{Old:%d, New:%d}, want {Old:0, New:7}", e.Old, e.New)
		}
		return nil
	}); err != nil {
		t.Errorf("expected HomeDERPChanged event: %v", err)
	}
}

func TestSetDERPMapDoReStun(t *testing.T) {
	derpMap1 := &tailcfg.DERPMap{
		Regions: map[int]*tailcfg.DERPRegion{
			1: {
				RegionID:   1,
				RegionCode: "cph",
				Nodes: []*tailcfg.DERPNode{
					{Name: "1a", RegionID: 1, HostName: "cph.test.unused", IPv4: "127.0.0.1", IPv6: "none"},
				},
			},
		},
	}
	derpMap2 := &tailcfg.DERPMap{
		Regions: map[int]*tailcfg.DERPRegion{
			2: {
				RegionID:   2,
				RegionCode: "inc",
				Nodes: []*tailcfg.DERPNode{
					{Name: "2a", RegionID: 2, HostName: "inc.test.unused", IPv4: "127.0.0.1", IPv6: "none"},
				},
			},
		},
	}

	var reSTUNCalls int
	tstest.Replace(t, &reSTUNHookForTests, func(_ string) {
		reSTUNCalls++
	})

	bus := eventbustest.NewBus(t)
	ht := health.NewTracker(bus)
	c := newConn(t.Logf)
	ec := bus.Client("magicsock.Conn.Test")
	c.eventClient = ec
	c.homeDERPChangedPub = eventbus.Publish[HomeDERPChanged](ec)
	c.eventBus = bus
	c.health = ht
	// With a zero private key and everHadKey=true, ReSTUN returns early without
	// spawning updateEndpoints.
	c.everHadKey = true

	// SetDERPMapWithoutReSTUN should not trigger a ReSTUN.
	c.SetDERPMapWithoutReSTUN(derpMap1)
	if reSTUNCalls != 0 {
		t.Errorf("SetDERPMapWithoutReSTUN: got %d ReSTUN calls, want 0", reSTUNCalls)
	}

	// SetDERPMap should trigger a ReSTUN.
	c.SetDERPMap(derpMap2)
	if reSTUNCalls != 1 {
		t.Errorf("SetDERPMap: got %d ReSTUN calls, want 1", reSTUNCalls)
	}
}
