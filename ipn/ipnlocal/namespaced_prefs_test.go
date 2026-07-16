// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package ipnlocal

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"tailscale.com/ipn"
)

type testNamespacedPref struct {
	Name  string
	Value string
}

func TestNamespacedPrefsStoreLoadSave(t *testing.T) {
	tests := []struct {
		name    string
		seed    map[string]testNamespacedPref // entries saved before the load
		loadKey string
		wantOK  bool
		want    testNamespacedPref
	}{
		{
			name:    "present_key_returns_value",
			seed:    map[string]testNamespacedPref{"a": {Name: "a", Value: "1"}},
			loadKey: "a",
			wantOK:  true,
			want:    testNamespacedPref{Name: "a", Value: "1"},
		},
		{
			name:    "absent_key_returns_zero",
			seed:    map[string]testNamespacedPref{"a": {Name: "a", Value: "1"}},
			loadKey: "missing",
			wantOK:  false,
			want:    testNamespacedPref{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newTestBackend(t)
			str, err := newNamespacedPrefsStore[testNamespacedPref](backend, "test-ns")
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.seed {
				if err := str.save(k, v); err != nil {
					t.Fatal(err)
				}
			}
			got, ok, err := str.load(tt.loadKey)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("value (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNamespacedPrefsStoreBehaviors(t *testing.T) {
	t.Run("overwrite_replaces_value", func(t *testing.T) {
		backend := newTestBackend(t)
		str, err := newNamespacedPrefsStore[testNamespacedPref](backend, "test-ns")
		if err != nil {
			t.Fatal(err)
		}
		if err := str.save("k", testNamespacedPref{Value: "first"}); err != nil {
			t.Fatal(err)
		}
		if err := str.save("k", testNamespacedPref{Value: "second"}); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := str.load("k"); got.Value != "second" {
			t.Errorf("after overwrite, got %q, want second", got.Value)
		}
	})

	t.Run("namespaces_isolated", func(t *testing.T) {
		backend := newTestBackend(t)
		nsA, err := newNamespacedPrefsStore[testNamespacedPref](backend, "ns-a")
		if err != nil {
			t.Fatal(err)
		}
		nsB, err := newNamespacedPrefsStore[testNamespacedPref](backend, "ns-b")
		if err != nil {
			t.Fatal(err)
		}
		if err := nsA.save("k", testNamespacedPref{Value: "a"}); err != nil {
			t.Fatal(err)
		}
		if err := nsB.save("k", testNamespacedPref{Value: "b"}); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := nsA.load("k"); got.Value != "a" {
			t.Errorf("ns-a[k] = %q, want a", got.Value)
		}
		if got, _, _ := nsB.load("k"); got.Value != "b" {
			t.Errorf("ns-b[k] = %q, want b", got.Value)
		}
	})

	t.Run("profiles_isolated", func(t *testing.T) {
		backend := newTestBackend(t)

		str, err := newNamespacedPrefsStore[testNamespacedPref](backend, "test-ns")
		if err != nil {
			t.Fatal(err)
		}
		if err := str.save("k", testNamespacedPref{Value: "profile0"}); err != nil {
			t.Fatal(err)
		}

		// Switch to a different profile; its store must not see profile0's data.
		backend.pm.currentProfile = (&ipn.LoginProfile{ID: "id1"}).View()
		str1, err := newNamespacedPrefsStore[testNamespacedPref](backend, "test-ns")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := str1.load("k"); ok {
			t.Error("second profile unexpectedly saw first profile's pref")
		}
	})

	t.Run("no_var_root_uses_memory", func(t *testing.T) {
		backend := newTestBackend(t)
		backend.SetVarRoot("") // force the in-memory fallback

		str, err := newNamespacedPrefsStore[testNamespacedPref](backend, "test-ns")
		if err != nil {
			t.Fatal(err)
		}
		if err := str.save("k", testNamespacedPref{Value: "v"}); err != nil {
			t.Fatal(err)
		}
		if got, ok, _ := str.load("k"); !ok || got.Value != "v" {
			t.Errorf("in-memory load = %q, ok=%v; want v, true", got.Value, ok)
		}
	})

	t.Run("evict_drops_cached_store", func(t *testing.T) {
		backend := newTestBackend(t)
		pid := backend.pm.CurrentProfile().ID()
		if _, err := newNamespacedPrefsStore[testNamespacedPref](backend, "test-ns"); err != nil {
			t.Fatal(err)
		}

		backend.mu.Lock()
		defer backend.mu.Unlock()
		if len(backend.namespacedPrefsStores) == 0 {
			t.Fatal("setup: expected a cached store")
		}
		backend.evictNamespacedPrefsStoresLocked(pid)
		if len(backend.namespacedPrefsStores) != 0 {
			t.Errorf("cache not emptied after evict: %v", backend.namespacedPrefsStores)
		}
	})
}
