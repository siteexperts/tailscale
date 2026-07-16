// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package ipnlocal

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"tailscale.com/ipn"
	"tailscale.com/ipn/store"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/util/mak"
)

// errNoCurrentProfile is returned when a namespaced prefs operation is attempted without a current
// login profile.
var errNoCurrentProfile = errors.New("no current profile")

// namespacedPrefsStore reads and writes the prefs for one namespace under one profile. Each namespace
// has its own file at profile-data/<id>/prefs/<namespace>.json, reusing [store.FileStore], which
// serves reads from an in-memory cache and writes atomically. It picks the profile's file when it's
// created and keeps using that same file, so if the user switches profiles between a load and a save,
// both still use the right one. T is the type of value stored under a key in the namespace.
type namespacedPrefsStore[T any] struct {
	store ipn.StateStore
}

// newNamespacedPrefsStore returns a store for reading and writing namespace's prefs under the current
// profile. It returns [errNoCurrentProfile] when there's no current profile.
func newNamespacedPrefsStore[T any](b *LocalBackend, namespace string) (*namespacedPrefsStore[T], error) {
	st, err := b.namespacedPrefsStateStore(namespace)
	if err != nil {
		return nil, err
	}
	return &namespacedPrefsStore[T]{store: st}, nil
}

// load reads and unmarshals the value stored under key. It reports ok=false when the key is absent,
// returning the zero value of T.
func (s *namespacedPrefsStore[T]) load(key string) (value T, ok bool, err error) {
	data, err := s.store.ReadState(ipn.StateKey(key))
	if errors.Is(err, ipn.ErrStateNotExist) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, false, err
	}
	return value, true, nil
}

// save marshals value and writes it under key.
func (s *namespacedPrefsStore[T]) save(key string, value T) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.store.WriteState(ipn.StateKey(key), data)
}

// namespacedPrefsStateStore returns the [ipn.StateStore] backing the given namespace for the current
// profile, creating it on first use and caching it. Each namespace gets its own file under
// profile-data/<id>/prefs/, isolated from the main prefs. When there's no writable storage
// (e.g. an ephemeral node), it falls back to an in-memory store so callers still work, and the data
// just doesn't persist across restarts.
func (b *LocalBackend) namespacedPrefsStateStore(namespace string) (ipn.StateStore, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	pid := b.pm.CurrentProfile().ID()
	if pid == "" {
		return nil, errNoCurrentProfile
	}

	cacheKey := string(pid) + "/" + namespace
	if st, ok := b.namespacedPrefsStores[cacheKey]; ok {
		return st, nil
	}

	st, err := b.newNamespacedPrefsStateStoreLocked(pid, namespace)
	if err != nil {
		return nil, err
	}
	mak.Set(&b.namespacedPrefsStores, cacheKey, st)
	return st, nil
}

// evictNamespacedPrefsStoresLocked drops cached namespaced prefs stores for pid, or for all profiles
// when pid is empty. It's called when a profile (or all profiles) is deleted so a later profile that
// reuses the same ID doesn't read a deleted profile's cached data. The caller must hold the mutex
// lock from the [LocalBackend].
func (b *LocalBackend) evictNamespacedPrefsStoresLocked(pid ipn.ProfileID) {
	if pid == "" {
		clear(b.namespacedPrefsStores)
		return
	}
	prefix := string(pid) + "/"
	for k := range b.namespacedPrefsStores {
		if strings.HasPrefix(k, prefix) {
			delete(b.namespacedPrefsStores, k)
		}
	}
}

// newNamespacedPrefsStateStoreLocked builds the [ipn.StateStore] for one namespace. It uses a file
// under profile-data/<id>/prefs/ when there's a writable storage path, or an in-memory store otherwise.
// The caller must hold the mutex lock from the [LocalBackend].
func (b *LocalBackend) newNamespacedPrefsStateStoreLocked(pid ipn.ProfileID, namespace string) (ipn.StateStore, error) {
	dir := b.profileDataPathLocked(pid, "prefs")
	if dir == "" {
		// No writable storage path when it's either an ephemeral node or a non-file based
		// [ipn.StateStore] (e.g. Kubernetes), so keep the data in memory only. Namespaced prefs
		// are at the moment only used by the desktop clients, so it's fine to return an
		// in-memory store in these cases, rather than erroring out.
		//
		// TODO(waltzofpearls): Persist to the node's [ipn.StateStore] backend (e.g. the
		// Kubernetes Secret) instead of an in-memory store, so namespaced prefs survive
		// restarts for cases like Kubernetes.
		return new(mem.Store), nil
	}
	path := filepath.Join(dir, namespace+".json")
	return store.NewFileStore(b.logf, path)
}
