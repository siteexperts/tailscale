// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package localapi

import "testing"

func TestNamespacedPrefsRouting(t *testing.T) {
	tests := []struct {
		path      string
		wantRoute string
	}{
		{"/localapi/v0/prefs", "/localapi/v0/prefs"},
		{"/localapi/v0/prefs/", "/localapi/v0/prefs/"},
		{"/localapi/v0/prefs/service-prefs", "/localapi/v0/prefs/"},
	}
	for _, tt := range tests {
		_, route, ok := handlerForPath(tt.path)
		if !ok {
			t.Errorf("%s: no handler matched", tt.path)
			continue
		}
		if route != tt.wantRoute {
			t.Errorf("%s: routed to %q, want %q", tt.path, route, tt.wantRoute)
		}
	}
}
