package main

// Regression tests for Nexus debt T006 (MCP-SESSION-LOSS-NO-RESUME).
//
// Operator-reported: every `make rebuild-restart` rotated child ports,
// invalidating sseSession.childPort. The keepalive loop closed the SSE
// with `child_died` and the operator had to manually `/mcp` reconnect.
//
// Fix: rebindSessionToNewChild updates sess.childPort when a new child
// for the same workspace is now serving on a different port. The handler
// reads sess.childPort fresh on every POST, so rebinding the struct alone
// is sufficient — the SSE stream survives the child rotation transparently.

import "testing"

// fakeLookup builds a closure that mimics lookupWorkspacePort with a
// fixed (workspaceID, port) mapping. Returns 0 for unknown IDs to mirror
// the production behavior on missing/cold workspaces.
func fakeLookup(known map[string]int) func(string) int {
	return func(wsID string) int {
		return known[wsID]
	}
}

func TestRebindSessionToNewChild_NoWorkspaceID_NoOp(t *testing.T) {
	sess := &sseSession{id: "s-1", childPort: 9105, workspaceID: ""}
	got := rebindSessionToNewChild(sess, fakeLookup(map[string]int{"ws-A": 9101}))
	if got != 0 {
		t.Errorf("expected 0 (cannot rebind without workspaceID), got %d", got)
	}
	if sess.childPort != 9105 {
		t.Errorf("childPort should be untouched, got %d", sess.childPort)
	}
}

func TestRebindSessionToNewChild_NoRunningChild_NoOp(t *testing.T) {
	sess := &sseSession{id: "s-2", childPort: 9105, workspaceID: "ws-B"}
	got := rebindSessionToNewChild(sess, fakeLookup(map[string]int{}))
	if got != 0 {
		t.Errorf("expected 0 (no running child), got %d", got)
	}
	if sess.childPort != 9105 {
		t.Errorf("childPort should be untouched on rebind miss, got %d", sess.childPort)
	}
}

func TestRebindSessionToNewChild_PortRotation_UpdatesSession(t *testing.T) {
	// Simulate post-rebuild: ws-C was on 9105, now on 9201.
	sess := &sseSession{id: "s-3", childPort: 9105, workspaceID: "ws-C"}
	got := rebindSessionToNewChild(sess, fakeLookup(map[string]int{"ws-C": 9201}))
	if got != 9201 {
		t.Errorf("expected resolved port 9201, got %d", got)
	}
	if sess.childPort != 9201 {
		t.Errorf("session childPort should be updated to 9201, got %d", sess.childPort)
	}
}

func TestRebindSessionToNewChild_SamePort_NoMutation(t *testing.T) {
	sess := &sseSession{id: "s-4", childPort: 9105, workspaceID: "ws-D"}
	got := rebindSessionToNewChild(sess, fakeLookup(map[string]int{"ws-D": 9105}))
	if got != 9105 {
		t.Errorf("expected 9105 (unchanged port), got %d", got)
	}
	if sess.childPort != 9105 {
		t.Errorf("childPort changed unexpectedly, got %d", sess.childPort)
	}
}
