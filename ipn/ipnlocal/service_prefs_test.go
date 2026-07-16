// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package ipnlocal

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
)

func TestServicePrefsSetAndGet(t *testing.T) {
	tests := []struct {
		name     string
		requests []apitype.ServicePrefRequest
		want     ipn.ServicePrefs
	}{
		{
			name:     "records_a_single_launch",
			requests: []apitype.ServicePrefRequest{{Key: "ssh:22", Client: "terminal", Username: "rollie"}},
			want:     ipn.ServicePrefs{"ssh:22": {Client: "terminal", Username: "rollie"}},
		},
		{
			name: "partial_update_preserves_existing_fields",
			requests: []apitype.ServicePrefRequest{
				{Key: "ssh:22", Client: "terminal", Username: "rollie"},
				{Key: "ssh:22", Client: "iterm2"},
			},
			want: ipn.ServicePrefs{"ssh:22": {Client: "iterm2", Username: "rollie"}},
		},
		{
			name: "independent_services_kept_separate",
			requests: []apitype.ServicePrefRequest{
				{Key: "ssh:22", Client: "terminal"},
				{Key: "db:5432", Client: "psql", DatabaseName: "prod"},
			},
			want: ipn.ServicePrefs{
				"ssh:22":  {Client: "terminal"},
				"db:5432": {Client: "psql", DatabaseName: "prod"},
			},
		},
	}

	ignoreLastUsed := cmpopts.IgnoreFields(ipn.ServicePref{}, "LastUsed")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newTestBackend(t)

			for _, req := range tt.requests {
				if _, err := backend.SetServicePref(req); err != nil {
					t.Fatal(err)
				}
			}

			got, err := backend.ServicePrefs()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got, ignoreLastUsed, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
			for key := range tt.want {
				if got[key].LastUsed.IsZero() {
					t.Errorf("%s: LastUsed was not stamped", key)
				}
			}
		})
	}
}

func TestServicePrefsPersistAcrossStoreReopen(t *testing.T) {
	backend := newTestBackend(t)
	if _, err := backend.SetServicePref(apitype.ServicePrefRequest{Key: "ssh:22", Client: "terminal"}); err != nil {
		t.Fatal(err)
	}

	// Drop the cached store so the next read reopens the file from disk, proving persistence.
	backend.mu.Lock()
	backend.namespacedPrefsStores = nil
	backend.mu.Unlock()

	got, err := backend.ServicePrefs()
	if err != nil {
		t.Fatal(err)
	}
	if got["ssh:22"].Client != "terminal" {
		t.Errorf("after reopen, got %v, want Client=terminal", got["ssh:22"])
	}
}

func TestServicePrefsEmptyKeyRejected(t *testing.T) {
	backend := newTestBackend(t)
	_, err := backend.SetServicePref(apitype.ServicePrefRequest{Client: "terminal"})
	if !errors.Is(err, ErrInvalidServicePref) {
		t.Errorf("want ErrInvalidServicePref for empty key, got %v", err)
	}
}

func TestServicePrefsNoCurrentProfileReturnsEmpty(t *testing.T) {
	backend := newTestBackend(t)
	backend.pm.currentProfile = (&ipn.LoginProfile{}).View() // no ID: logged out

	got, err := backend.ServicePrefs()
	if err != nil {
		t.Fatalf("want nil error for no current profile, got %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("want empty non-nil map, got %#v", got)
	}
}

func TestServicePrefsEvictedForDeletedProfile(t *testing.T) {
	backend := newTestBackend(t)
	pid := backend.pm.CurrentProfile().ID()

	if _, err := backend.SetServicePref(apitype.ServicePrefRequest{Key: "ssh:22", Client: "terminal"}); err != nil {
		t.Fatal(err)
	}
	// The store for this profile should now be cached.
	backend.mu.Lock()
	if len(backend.namespacedPrefsStores) == 0 {
		backend.mu.Unlock()
		t.Fatal("setup: expected a cached store")
	}
	// Evict as DeleteProfile does, then confirm the cache no longer holds this profile.
	backend.evictNamespacedPrefsStoresLocked(pid)
	for k := range backend.namespacedPrefsStores {
		if strings.HasPrefix(k, string(pid)+"/") {
			backend.mu.Unlock()
			t.Fatalf("cache entry %q for deleted profile %q was not evicted", k, pid)
		}
	}
	backend.mu.Unlock()
}
