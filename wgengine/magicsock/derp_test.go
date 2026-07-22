// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package magicsock

import (
	"fmt"
	"testing"

	"tailscale.com/health"
	"tailscale.com/net/netcheck"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest"
	"tailscale.com/types/key"
	"tailscale.com/util/eventbus"
	"tailscale.com/util/eventbus/eventbustest"
)

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

func TestAuthoritativeDERPRoutePreservesUnrelatedPeers(t *testing.T) {
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

	if err := c.SetNetworkMapWithDERPRoutePolicy(tailcfg.NodeView{}, peers, []AuthoritativeDERPRoute{{Peer: authoritativePeer, RegionID: 901, Generation: 1}}); err != nil {
		t.Fatal(err)
	}
	if got := c.fallbackDERPRegionForPeer(authoritativePeer); got != 0 {
		t.Errorf("authoritative peer fallback DERP region = %d, want 0", got)
	}
	if got := c.fallbackDERPRegionForPeer(otherPeer); got != 801 {
		t.Errorf("unrelated peer fallback DERP region = %d, want 801", got)
	}

	c.addDerpPeerRoute(otherPeer, 802, nil)
	if got := c.fallbackDERPRegionForPeer(otherPeer); got != 802 {
		t.Errorf("unrelated peer relearned DERP region = %d, want 802", got)
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
