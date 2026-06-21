// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnabledGate pins the auto-update opt-in semantics: empty StatePath is
// legacy always-on; a configured StatePath is OFF by default (missing/false/
// malformed) and only on when the file says {"enabled": true}.
func TestEnabledGate(t *testing.T) {
	if !New(Config{}).enabled() {
		t.Error("empty StatePath must be enabled (legacy always-on)")
	}
	sp := filepath.Join(t.TempDir(), "auto-update.json")
	u := New(Config{StatePath: sp})
	if u.enabled() {
		t.Error("missing state file must be DISABLED (off by default)")
	}
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"enabled": false}`, false},
		{`{"enabled": true}`, true},
		{`not json`, false},
		{`{}`, false},
	} {
		if err := os.WriteFile(sp, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := u.enabled(); got != tc.want {
			t.Errorf("state %q: enabled()=%v want %v", tc.body, got, tc.want)
		}
	}
}
