package main

import (
	"context"
	"testing"

	"github.com/ensamblatec/neoanvil/pkg/plugin"
)

func TestDiffManifestSpecs_Added(t *testing.T) {
	old := []*plugin.PluginSpec{
		{Name: "jira", Binary: "/a", NamespacePrefix: "jira"},
	}
	newer := []*plugin.PluginSpec{
		{Name: "jira", Binary: "/a", NamespacePrefix: "jira"},
		{Name: "github", Binary: "/b", NamespacePrefix: "gh"},
	}
	added, removed, changed := diffManifestSpecs(old, newer)
	if len(added) != 1 || added[0].Name != "github" {
		t.Errorf("added=%+v want [github]", added)
	}
	if len(removed) != 0 || len(changed) != 0 {
		t.Errorf("expected no removed/changed, got %v / %v", removed, changed)
	}
}

func TestDiffManifestSpecs_Removed(t *testing.T) {
	old := []*plugin.PluginSpec{
		{Name: "jira", Binary: "/a"},
		{Name: "github", Binary: "/b"},
	}
	newer := []*plugin.PluginSpec{
		{Name: "jira", Binary: "/a"},
	}
	added, removed, changed := diffManifestSpecs(old, newer)
	if len(removed) != 1 || removed[0].Name != "github" {
		t.Errorf("removed=%+v want [github]", removed)
	}
	if len(added) != 0 || len(changed) != 0 {
		t.Errorf("expected no added/changed")
	}
}

func TestDiffManifestSpecs_ChangedBinary(t *testing.T) {
	old := []*plugin.PluginSpec{{Name: "jira", Binary: "/a"}}
	newer := []*plugin.PluginSpec{{Name: "jira", Binary: "/new-binary"}}
	_, _, changed := diffManifestSpecs(old, newer)
	if len(changed) != 1 {
		t.Errorf("changed=%d want 1", len(changed))
	}
}

func TestDiffManifestSpecs_ChangedArgs(t *testing.T) {
	old := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", Args: []string{"--v1"}}}
	newer := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", Args: []string{"--v2"}}}
	_, _, changed := diffManifestSpecs(old, newer)
	if len(changed) != 1 {
		t.Errorf("changed args should trigger restart: changed=%d", len(changed))
	}
}

func TestDiffManifestSpecs_ChangedEnvFromVault(t *testing.T) {
	old := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", EnvFromVault: []string{"JIRA_TOKEN"}}}
	newer := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", EnvFromVault: []string{"JIRA_TOKEN", "JIRA_EMAIL"}}}
	_, _, changed := diffManifestSpecs(old, newer)
	if len(changed) != 1 {
		t.Errorf("env_from_vault change should trigger restart")
	}
}

func TestDiffManifestSpecs_ChangedNamespacePrefix(t *testing.T) {
	old := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", NamespacePrefix: "jira"}}
	newer := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", NamespacePrefix: "atlassian"}}
	_, _, changed := diffManifestSpecs(old, newer)
	if len(changed) != 1 {
		t.Errorf("namespace_prefix change should trigger restart")
	}
}

func TestDiffManifestSpecs_NoChange(t *testing.T) {
	old := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", NamespacePrefix: "jira"}}
	newer := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", NamespacePrefix: "jira"}}
	added, removed, changed := diffManifestSpecs(old, newer)
	if len(added)+len(removed)+len(changed) != 0 {
		t.Errorf("identical specs should yield zero diff: +%d -%d ~%d",
			len(added), len(removed), len(changed))
	}
}

func TestDiffManifestSpecs_DescriptionChangeIgnored(t *testing.T) {
	old := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", Description: "old"}}
	newer := []*plugin.PluginSpec{{Name: "jira", Binary: "/a", Description: "new"}}
	_, _, changed := diffManifestSpecs(old, newer)
	if len(changed) != 0 {
		t.Errorf("description change should NOT trigger restart")
	}
}

func TestDiffManifestSpecs_NilEntriesSkipped(t *testing.T) {
	old := []*plugin.PluginSpec{nil, {Name: "jira", Binary: "/a"}}
	newer := []*plugin.PluginSpec{{Name: "jira", Binary: "/a"}, nil}
	added, removed, changed := diffManifestSpecs(old, newer)
	if len(added) != 0 || len(removed) != 0 || len(changed) != 0 {
		t.Errorf("nil entries should be skipped: +%d -%d ~%d", len(added), len(removed), len(changed))
	}
}

func TestStringSliceEqual(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{}, []string{}, true},
		{nil, []string{}, true},
		{[]string{"a"}, []string{"a"}, true},
		{[]string{"a", "b"}, []string{"a", "b"}, true},
		{[]string{"a", "b"}, []string{"b", "a"}, false},
		{[]string{"a"}, []string{"a", "b"}, false},
	}
	for _, tc := range cases {
		if got := stringSliceEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("stringSliceEqual(%v,%v)=%v want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestRemoveConnAndToolsLocked(t *testing.T) {
	rt := &pluginRuntime{
		conns: []plugin.Connected{
			{Name: "jira"},
			{Name: "github"},
			{Name: "jira"}, // duplicate edge case
		},
		tools: []plugin.NamespacedTool{
			{PluginName: "jira", Tool: plugin.Tool{Name: "a"}},
			{PluginName: "github", Tool: plugin.Tool{Name: "b"}},
			{PluginName: "jira", Tool: plugin.Tool{Name: "c"}},
		},
		errors: map[string]error{},
	}
	rt.removeConnAndToolsLocked("jira")
	if len(rt.conns) != 1 || rt.conns[0].Name != "github" {
		t.Errorf("conns after remove: %+v want [github]", rt.conns)
	}
	if len(rt.tools) != 1 || rt.tools[0].PluginName != "github" {
		t.Errorf("tools after remove: %+v want [github/b]", rt.tools)
	}
}

func TestReload_NilRuntimeReturnsError(t *testing.T) {
	var rt *pluginRuntime
	if err := rt.reload(context.TODO()); err == nil {
		t.Error("reload on nil runtime should error")
	}
	rt2 := &pluginRuntime{}
	if err := rt2.reload(context.TODO()); err == nil {
		t.Error("reload on runtime without pool should error")
	}
}
