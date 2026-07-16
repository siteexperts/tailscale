// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package localapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/util/httpm"
)

// serveServicePrefs handles GET and POST /localapi/v0/prefs/service-prefs. GET returns all of
// the current profile's service prefs. POST merges one [apitype.ServicePrefRequest] into the
// saved service prefs and returns the full updated set.
func (h *Handler) serveServicePrefs(w http.ResponseWriter, r *http.Request) {
	if !h.PermitRead {
		http.Error(w, "service-prefs access denied", http.StatusForbidden)
		return
	}
	var out ipn.ServicePrefs
	switch r.Method {
	case httpm.GET:
		var err error
		if out, err = h.b.ServicePrefs(); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(resJSON{Error: err.Error()})
			return
		}
	case httpm.POST:
		if !h.PermitWrite {
			http.Error(w, "service-prefs write access denied", http.StatusForbidden)
			return
		}
		var req apitype.ServicePrefRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var err error
		if out, err = h.b.SetServicePref(req); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ipnlocal.ErrInvalidServicePref) {
				status = http.StatusBadRequest
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(resJSON{Error: err.Error()})
			return
		}
	default:
		http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
