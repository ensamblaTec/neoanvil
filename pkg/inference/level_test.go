package inference

import "testing"

// TestLevel_String [Épica 231.C]
func TestLevel_String(t *testing.T) {
	cases := []struct {
		lvl  Level
		want string
	}{
		{LOCAL, "LOCAL"},
		{OLLAMA, "OLLAMA"},
		{HYBRID, "HYBRID"},
		{CLOUD, "CLOUD"},
		{Level(99), "UNKNOWN"},
	}
	for _, c := range cases {
		if got := c.lvl.String(); got != c.want {
			t.Errorf("Level(%d).String() = %q, want %q", c.lvl, got, c.want)
		}
	}
}
