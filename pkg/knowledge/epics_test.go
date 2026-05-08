package knowledge

import (
	"errors"
	"slices"
	"testing"
)

// TestValidateEpicID covers the '<PILAR>-<id>' format rule. [332.A]
func TestValidateEpicID(t *testing.T) {
	cases := []struct {
		key     string
		wantErr bool
	}{
		{"LX-332.A", false},
		{"LVII-328.C", false},
		{"LXIX-400.Z", false},
		{"X-1", false},
		{"", true},          // empty
		{"-332.A", true},    // empty PILAR
		{"LX-", true},       // empty id
		{"LX332A", true},    // no separator
	}
	for _, c := range cases {
		err := ValidateEpicID(c.key)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateEpicID(%q) = %v, wantErr=%v", c.key, err, c.wantErr)
		}
	}
}

// TestValidateEpicStatus accepts empty + 4 valid statuses. [332.A]
func TestValidateEpicStatus(t *testing.T) {
	valid := []string{"", EpicStatusOpen, EpicStatusInProgress, EpicStatusDone, EpicStatusBlocked}
	for _, s := range valid {
		if err := ValidateEpicStatus(s); err != nil {
			t.Errorf("ValidateEpicStatus(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{"OPEN", "complete", "wip", "pending"}
	for _, s := range invalid {
		if err := ValidateEpicStatus(s); !errors.Is(err, ErrEpicInvalidStatus) {
			t.Errorf("ValidateEpicStatus(%q) = %v, want ErrEpicInvalidStatus", s, err)
		}
	}
}

// TestValidateEpicPriority accepts empty + P0-P3. [332.A]
func TestValidateEpicPriority(t *testing.T) {
	valid := []string{"", EpicPriorityP0, EpicPriorityP1, EpicPriorityP2, EpicPriorityP3}
	for _, p := range valid {
		if err := ValidateEpicPriority(p); err != nil {
			t.Errorf("ValidateEpicPriority(%q) = %v, want nil", p, err)
		}
	}
	invalid := []string{"p0", "P4", "high", "1", "P-1"}
	for _, p := range invalid {
		if err := ValidateEpicPriority(p); !errors.Is(err, ErrEpicInvalidPriority) {
			t.Errorf("ValidateEpicPriority(%q) = %v, want ErrEpicInvalidPriority", p, err)
		}
	}
}

// TestPutEpic_HappyPath writes and reads back an epic entry. [332.A]
func TestPutEpic_HappyPath(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()

	err := ks.PutEpic(
		"LX-332.A",
		"Namespace epics with validated schema",
		"Add NSEpics namespace with schema validation and cross-workspace sync",
		EpicStatusInProgress,
		EpicPriorityP1,
		"neoanvil-95248",
		[]string{"strategosia-28463"},
		nil,
	)
	if err != nil {
		t.Fatalf("PutEpic: %v", err)
	}

	got, err := ks.Get(NSEpics, "LX-332.A")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.EpicTitle != "Namespace epics with validated schema" {
		t.Errorf("EpicTitle = %q", got.EpicTitle)
	}
	if got.EpicStatus != EpicStatusInProgress {
		t.Errorf("EpicStatus = %q", got.EpicStatus)
	}
	if got.Priority != EpicPriorityP1 {
		t.Errorf("Priority = %q", got.Priority)
	}
	if got.EpicOwner != "neoanvil-95248" {
		t.Errorf("EpicOwner = %q", got.EpicOwner)
	}
	if len(got.EpicAffected) != 1 || got.EpicAffected[0] != "strategosia-28463" {
		t.Errorf("EpicAffected = %v", got.EpicAffected)
	}
}

// TestPutEpic_DefaultsStatus — empty status becomes "open". [332.A]
func TestPutEpic_DefaultsStatus(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	if err := ks.PutEpic("LX-332.B", "title", "content", "", "", "owner", nil, nil); err != nil {
		t.Fatalf("PutEpic: %v", err)
	}
	got, _ := ks.Get(NSEpics, "LX-332.B")
	if got.EpicStatus != EpicStatusOpen {
		t.Errorf("default status = %q, want 'open'", got.EpicStatus)
	}
}

// TestPutEpic_InvalidID rejects bad key format. [332.A]
func TestPutEpic_InvalidID(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	_ = ks.PutEpic("bad-key-without-pilar-structure", "", "", "", "", "", nil, nil)
	// ValidateEpicID("bad-key-without-pilar-structure") → ok (has '-'), but valid format
	// Let's test a truly invalid one.
	err := ks.PutEpic("-332.A", "", "", "", "", "", nil, nil)
	if !errors.Is(err, ErrEpicInvalidID) {
		t.Errorf("want ErrEpicInvalidID, got %v", err)
	}
}

// TestPutEpic_InvalidStatus rejects unknown status. [332.A]
func TestPutEpic_InvalidStatus(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	err := ks.PutEpic("LX-332.A", "", "", "wip", "", "", nil, nil)
	if !errors.Is(err, ErrEpicInvalidStatus) {
		t.Errorf("want ErrEpicInvalidStatus, got %v", err)
	}
}

// TestPutEpic_InvalidPriority rejects unknown priority. [332.A]
func TestPutEpic_InvalidPriority(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()
	err := ks.PutEpic("LX-332.A", "", "", "", "P4", "", nil, nil)
	if !errors.Is(err, ErrEpicInvalidPriority) {
		t.Errorf("want ErrEpicInvalidPriority, got %v", err)
	}
}

// TestPut_EnforcesEpicsSchemaAtGenericLevel — generic Put also validates. [332.A]
func TestPut_EnforcesEpicsSchemaAtGenericLevel(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()

	// Bad key
	err := ks.Put(NSEpics, "-bad", KnowledgeEntry{Content: "x"})
	if !errors.Is(err, ErrEpicInvalidID) {
		t.Errorf("Put with bad epic key: want ErrEpicInvalidID, got %v", err)
	}

	// Bad status
	err = ks.Put(NSEpics, "LX-1", KnowledgeEntry{EpicStatus: "wip"})
	if !errors.Is(err, ErrEpicInvalidStatus) {
		t.Errorf("Put with bad EpicStatus: want ErrEpicInvalidStatus, got %v", err)
	}

	// Bad priority
	err = ks.Put(NSEpics, "LX-1", KnowledgeEntry{Priority: "P9"})
	if !errors.Is(err, ErrEpicInvalidPriority) {
		t.Errorf("Put with bad Priority: want ErrEpicInvalidPriority, got %v", err)
	}
}

// TestListEpicsByStatus filters correctly. [332.A]
func TestListEpicsByStatus(t *testing.T) {
	ks, _ := Open(t.TempDir() + "/k.db")
	defer ks.Close()

	_ = ks.PutEpic("LX-332.A", "a", "", EpicStatusInProgress, "", "owner", nil, nil)
	_ = ks.PutEpic("LX-332.B", "b", "", EpicStatusOpen, "", "owner", nil, nil)
	_ = ks.PutEpic("LX-332.C", "c", "", EpicStatusDone, "", "owner", nil, nil)
	_ = ks.PutEpic("LX-333.A", "d", "", EpicStatusOpen, "", "owner", nil, nil)

	open, err := ks.ListEpicsByStatus(EpicStatusOpen)
	if err != nil {
		t.Fatalf("ListEpicsByStatus(open): %v", err)
	}
	if len(open) != 2 {
		t.Errorf("want 2 open epics, got %d", len(open))
	}

	done, _ := ks.ListEpicsByStatus(EpicStatusDone)
	if len(done) != 1 || done[0].Key != "LX-332.C" {
		t.Errorf("want 1 done epic LX-332.C, got %v", done)
	}

	all, _ := ks.ListEpicsByStatus("")
	if len(all) != 4 {
		t.Errorf("want 4 total epics, got %d", len(all))
	}
}

// TestReservedNamespacesIncludesEpics — NSEpics must be in reserved list. [332.A]
func TestReservedNamespacesIncludesEpics(t *testing.T) {
	if !slices.Contains(ReservedNamespaces(), NSEpics) {
		t.Error("ReservedNamespaces() must include NSEpics")
	}
}
