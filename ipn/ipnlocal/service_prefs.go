// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package ipnlocal

import (
	"errors"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
)

// servicePrefsNamespace is the /prefs/{namespace} namespace for desktop service launch prefs.
const servicePrefsNamespace = "service-prefs"

// servicePrefsKey is the single key under which the entire [ipn.ServicePrefs] map is stored.
const servicePrefsKey = "service-prefs"

// ErrInvalidServicePref is returned by [LocalBackend.SetServicePref] for a request that fails
// validation, such as a missing key. Handlers map it to a 400, unlike backend failures.
var ErrInvalidServicePref = errors.New("service pref key is required")

// ServicePrefs returns the saved service prefs for the current profile. It returns an empty,
// non-nil map when nothing has been saved yet, or when there's no current profile (a logged out
// node simply has no service prefs).
func (b *LocalBackend) ServicePrefs() (ipn.ServicePrefs, error) {
	str, err := newNamespacedPrefsStore[ipn.ServicePrefs](b, servicePrefsNamespace)
	if errors.Is(err, errNoCurrentProfile) {
		return ipn.ServicePrefs{}, nil
	}
	if err != nil {
		return nil, err
	}
	prefs, ok, err := str.load(servicePrefsKey)
	if err != nil {
		return nil, err
	}
	if !ok || prefs == nil {
		return ipn.ServicePrefs{}, nil
	}
	return prefs, nil
}

// SetServicePref merges the non-empty fields from [apitype.ServicePrefRequest] into the saved
// service pref for its key, stamps [ipn.ServicePref.LastUsed] with the current time, and returns
// the full updated service prefs for the current profile.
func (b *LocalBackend) SetServicePref(req apitype.ServicePrefRequest) (ipn.ServicePrefs, error) {
	if req.Key == "" {
		return nil, ErrInvalidServicePref
	}

	// Serialize the read-modify-write so concurrent writes don't lose each other's updates.
	b.servicePrefsWriteMu.Lock()
	defer b.servicePrefsWriteMu.Unlock()

	str, err := newNamespacedPrefsStore[ipn.ServicePrefs](b, servicePrefsNamespace)
	if err != nil {
		return nil, err
	}
	prefs, ok, err := str.load(servicePrefsKey)
	if err != nil {
		return nil, err
	}
	if !ok || prefs == nil {
		prefs = ipn.ServicePrefs{}
	}

	pref := prefs[req.Key]
	if req.Client != "" {
		pref.Client = req.Client
	}
	if req.Username != "" {
		pref.Username = req.Username
	}
	if req.DatabaseName != "" {
		pref.DatabaseName = req.DatabaseName
	}
	pref.LastUsed = b.clock.Now()
	prefs[req.Key] = pref

	if err := str.save(servicePrefsKey, prefs); err != nil {
		return nil, err
	}
	return prefs, nil
}
