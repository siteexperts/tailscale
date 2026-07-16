// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package localapi

import (
	"net/http"
	"strings"
)

// namespacedPrefsHandlers holds a list of the LocalAPI handlers for each /localapi/v0/prefs/{namespace}.
var namespacedPrefsHandlers = map[string]LocalAPIHandler{
	"service-prefs": (*Handler).serveServicePrefs,
}

// serveNamespacedPrefs dispatches /localapi/v0/prefs/{namespace} to the handler registered for that
// namespace. An empty namespace (/localapi/v0/prefs/) is delegated to servePrefs so the existing
// [ipn.Prefs] endpoint is unchanged. An unregistered namespace gets a 404 back.
func (h *Handler) serveNamespacedPrefs(w http.ResponseWriter, r *http.Request) {
	namespace, _ := strings.CutPrefix(r.URL.EscapedPath(), "/localapi/v0/prefs/")
	if namespace == "" {
		h.servePrefs(w, r)
		return
	}
	fn, ok := namespacedPrefsHandlers[namespace]
	if !ok {
		http.Error(w, "unknown prefs namespace", http.StatusNotFound)
		return
	}
	fn(h, w, r)
}
