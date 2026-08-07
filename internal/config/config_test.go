package config

import "testing"

// ArchiveEnabled defaults to ON when the pointer is nil (absent key) or explicitly true, and OFF only
// when explicitly false. The bot reads config at startup, so this toggle must be resolvable in-process
// (an E2E config-flip cannot re-enable/disable a running bot).
func TestArchiveEnabled(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		ptr  *bool
		want bool
	}{
		{"nil defaults on", nil, true},
		{"explicit true", &on, true},
		{"explicit false", &off, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (AppConfig{MessageArchiveEnabled: tc.ptr}).ArchiveEnabled(); got != tc.want {
				t.Errorf("ArchiveEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// BusyIndicatorEnabled defaults to ON when the pointer is nil (absent key) or explicitly true, and
// OFF only when explicitly set false.
func TestBusyIndicatorEnabled(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		ptr  *bool
		want bool
	}{
		{"nil defaults on", nil, true},
		{"explicit true", &on, true},
		{"explicit false", &off, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (AppConfig{BusyIndicator: tc.ptr}).BusyIndicatorEnabled(); got != tc.want {
				t.Errorf("BusyIndicatorEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
