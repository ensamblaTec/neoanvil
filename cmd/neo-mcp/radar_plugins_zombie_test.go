package main

import (
	"testing"
	"time"
)

// TestPluginHealthBrief_IsZombie covers the three signals that trigger
// the zombie marker in BRIEFING. [ÉPICA 152.C]
func TestPluginHealthBrief_IsZombie(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name string
		h    *pluginHealthBrief
		want bool
	}{
		{
			name: "nil_no_data",
			h:    nil,
			want: false, // no health snapshot yet — don't crash, don't claim zombie
		},
		{
			name: "healthy",
			h: &pluginHealthBrief{
				Alive:           true,
				ToolsRegistered: []string{"call"},
				PolledAtUnix:    now,
			},
			want: false,
		},
		{
			name: "poll_err_set",
			h: &pluginHealthBrief{
				PolledAtUnix: now,
				PollErr:      "decode failure",
			},
			want: true,
		},
		{
			name: "tools_empty",
			h: &pluginHealthBrief{
				Alive:           true,
				ToolsRegistered: []string{}, // dispatcher loaded but registry empty
				PolledAtUnix:    now,
			},
			want: true,
		},
		{
			name: "stale_poll",
			h: &pluginHealthBrief{
				Alive:           true,
				ToolsRegistered: []string{"call"},
				PolledAtUnix:    now - 120, // 2 minutes ago — older than 90s threshold
			},
			want: true,
		},
		{
			name: "borderline_stale",
			h: &pluginHealthBrief{
				Alive:           true,
				ToolsRegistered: []string{"call"},
				PolledAtUnix:    now - 60, // exactly 60s ago — under 90s threshold
			},
			want: false,
		},
		{
			name: "no_poll_yet_initial",
			h: &pluginHealthBrief{
				Alive:           true,
				ToolsRegistered: []string{"call"},
				PolledAtUnix:    0, // never polled — bootstrapping period
			},
			want: false, // PolledAtUnix=0 short-circuits stale check
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.IsZombie(); got != tc.want {
				t.Errorf("IsZombie() = %v, want %v", got, tc.want)
			}
		})
	}
}
