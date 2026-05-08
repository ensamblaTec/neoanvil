package knowledge

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestValidateInboxKey covers the `to-<ws-id>-<topic>` format rule. [331.A]
func TestValidateInboxKey(t *testing.T) {
	cases := []struct {
		key     string
		wantErr bool
	}{
		{"to-strategos-32492-api-v2", false},
		{"to-strategos-32492-x", false},
		{"to-ws-topic", false},
		{"strategos-32492/foo", true},    // missing to- prefix
		{"to-", true},                    // empty after prefix
		{"to-strategos", true},           // no - separator between ws/topic
		{"to--topic", true},              // empty ws-id (no char before first -)
		{"to-ws-", true},                 // empty topic
		{"", true},                       // empty string
	}
	for _, c := range cases {
		err := ValidateInboxKey(c.key)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateInboxKey(%q) = %v, wantErr=%v", c.key, err, c.wantErr)
		}
	}
}

// TestValidateInboxPriority accepts empty + 3 valid priorities. [331.A]
func TestValidateInboxPriority(t *testing.T) {
	valid := []string{"", "low", "normal", "urgent"}
	for _, p := range valid {
		if err := ValidateInboxPriority(p); err != nil {
			t.Errorf("ValidateInboxPriority(%q) = %v, want nil", p, err)
		}
	}
	invalid := []string{"high", "NORMAL", "LOW", "x", "medium"}
	for _, p := range invalid {
		if err := ValidateInboxPriority(p); !errors.Is(err, ErrInboxInvalidPriority) {
			t.Errorf("ValidateInboxPriority(%q) = %v, want ErrInboxInvalidPriority", p, err)
		}
	}
}

// TestPutInbox_HappyPath — valid sender+key+priority stores successfully. [331.A]
func TestPutInbox_HappyPath(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	err := ks.PutInbox("strategos-32492", "to-strategosia-frontend-82899-api-change", "role is now required", "urgent", 0)
	if err != nil {
		t.Fatalf("PutInbox: %v", err)
	}
	got, err := ks.Get(NSInbox, "to-strategosia-frontend-82899-api-change")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.From != "strategos-32492" {
		t.Errorf("From = %q", got.From)
	}
	if got.Priority != "urgent" {
		t.Errorf("Priority = %q", got.Priority)
	}
	if got.ReadAt != 0 {
		t.Errorf("ReadAt = %d, want 0 (unread)", got.ReadAt)
	}
}

// TestPutInbox_MissingFrom rejects empty sender. [331.A]
func TestPutInbox_MissingFrom(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	err := ks.PutInbox("", "to-ws-topic", "body", "", 0)
	if !errors.Is(err, ErrInboxMissingFrom) {
		t.Errorf("want ErrInboxMissingFrom, got %v", err)
	}
}

// TestPutInbox_InvalidKey rejects wrong format. [331.A]
func TestPutInbox_InvalidKey(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	err := ks.PutInbox("sender", "not-a-valid-inbox-key", "body", "", 0)
	if !errors.Is(err, ErrInboxInvalidKey) {
		t.Errorf("want ErrInboxInvalidKey, got %v", err)
	}
}

// TestPut_EnforcesInboxKeyFormatAtGenericLevel — even generic Put validates. [331.A]
func TestPut_EnforcesInboxKeyFormatAtGenericLevel(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	err := ks.Put(NSInbox, "raw-garbage-key", KnowledgeEntry{Content: "body"})
	if !errors.Is(err, ErrInboxInvalidKey) {
		t.Errorf("generic Put for inbox must reject invalid key, got %v", err)
	}
}

// TestPutInbox_QuotaEnforced trips after N messages from same sender. [331.A]
func TestPutInbox_QuotaEnforced(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	const cap = 3
	for i := range cap {
		key := "to-target-topic" + string(rune('0'+i))
		if err := ks.PutInbox("spammer", key, "body", "", cap); err != nil {
			t.Fatalf("call %d within quota should succeed: %v", i, err)
		}
	}
	// Cap+1 must fail.
	err := ks.PutInbox("spammer", "to-target-over", "body", "", cap)
	if !errors.Is(err, ErrInboxQuotaExceeded) {
		t.Errorf("want ErrInboxQuotaExceeded, got %v", err)
	}
	// Different sender under the cap should still work.
	if err := ks.PutInbox("polite-sender", "to-target-from-polite", "body", "", cap); err != nil {
		t.Errorf("different sender should not be rate-limited: %v", err)
	}
}

// TestPutInbox_IdempotentReWriteNotQuoted — updating the SAME key doesn't count. [331.A]
func TestPutInbox_IdempotentReWriteNotQuoted(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	const cap = 2
	key := "to-target-same-key"
	for i := range cap + 3 {
		if err := ks.PutInbox("sender", key, "body", "", cap); err != nil {
			t.Fatalf("re-write %d should succeed (idempotent), got %v", i, err)
		}
	}
}

// TestMarkInboxRead sets ReadAt. [331.A]
func TestMarkInboxRead(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	key := "to-ws-topic"
	_ = ks.PutInbox("sender", key, "body", "", 0)
	if err := ks.MarkInboxRead(key); err != nil {
		t.Fatalf("MarkInboxRead: %v", err)
	}
	got, _ := ks.Get(NSInbox, key)
	if got.ReadAt == 0 {
		t.Error("ReadAt should be non-zero after MarkInboxRead")
	}
}

// TestListInboxFor filters by target workspace ID + unread. [331.A]
func TestListInboxFor(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	// Two messages TO different targets.
	_ = ks.PutInbox("s1", "to-target-a-topic1", "body", "", 0)
	_ = ks.PutInbox("s1", "to-target-a-topic2", "body", "", 0)
	_ = ks.PutInbox("s1", "to-target-b-topic1", "body", "", 0)
	// Mark one of target-a as read.
	_ = ks.MarkInboxRead("to-target-a-topic1")

	// ListInboxFor("target-a") → 2 total
	all, err := ks.ListInboxFor("target-a", false)
	if err != nil {
		t.Fatalf("ListInboxFor(all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("want 2 entries for target-a, got %d", len(all))
	}
	// ListInboxFor("target-a", unreadOnly=true) → 1
	unread, _ := ks.ListInboxFor("target-a", true)
	if len(unread) != 1 {
		t.Errorf("want 1 unread entry, got %d", len(unread))
	}
	if unread[0].Key != "to-target-a-topic2" {
		t.Errorf("unread entry should be topic2, got %q", unread[0].Key)
	}
	// target-b sees only its own.
	b, _ := ks.ListInboxFor("target-b", false)
	if len(b) != 1 {
		t.Errorf("target-b should see 1 entry, got %d", len(b))
	}
}

// TestListInboxFor_EmptyTarget rejects empty input. [331.A]
func TestListInboxFor_EmptyTarget(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	_, err := ks.ListInboxFor("", false)
	if err == nil || !strings.Contains(err.Error(), "targetWSID is required") {
		t.Errorf("expected 'targetWSID is required' error, got %v", err)
	}
}

// TestReservedNamespacesIncludesInbox — 330.J list must include NSInbox. [331.A]
func TestReservedNamespacesIncludesInbox(t *testing.T) {
	if !slices.Contains(ReservedNamespaces(), NSInbox) {
		t.Error("ReservedNamespaces() must include NSInbox")
	}
}
