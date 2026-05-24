package contacts

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "contacts.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAddAndSearch(t *testing.T) {
	db := newTestDB(t)

	if err := db.Add("alice@example.com", "Alice", SourceImported); err != nil {
		t.Fatalf("add: %v", err)
	}

	results, err := db.Search("alice", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Email != "alice@example.com" {
		t.Errorf("email = %q, want %q", results[0].Email, "alice@example.com")
	}
	if results[0].Name != "Alice" {
		t.Errorf("name = %q, want %q", results[0].Name, "Alice")
	}
}

func TestAdd_NormalizesEmail(t *testing.T) {
	db := newTestDB(t)
	db.Add("Alice@Example.COM", "Alice", SourceImported)

	results, _ := db.Search("alice", 10)
	if len(results) != 1 {
		t.Fatalf("got %d, want 1", len(results))
	}
	if results[0].Email != "alice@example.com" {
		t.Errorf("email = %q, want lowercase", results[0].Email)
	}
}

func TestAdd_InvalidEmail(t *testing.T) {
	db := newTestDB(t)
	err := db.Add("not-an-email", "Bad", SourceManual)
	if err == nil {
		t.Error("expected error for invalid email")
	}
}

func TestAdd_UpsertUpdatesName(t *testing.T) {
	db := newTestDB(t)
	db.Add("alice@example.com", "", SourceImported)
	db.Add("alice@example.com", "Alice Smith", SourceImported)

	results, _ := db.Search("alice", 10)
	if len(results) != 1 {
		t.Fatalf("got %d, want 1 (upsert should not duplicate)", len(results))
	}
	if results[0].Name != "Alice Smith" {
		t.Errorf("name = %q, want updated name", results[0].Name)
	}
}

func TestAdd_UpsertDoesNotClearName(t *testing.T) {
	db := newTestDB(t)
	db.Add("alice@example.com", "Alice", SourceImported)
	db.Add("alice@example.com", "", SourceImported) // empty name should not overwrite

	results, _ := db.Search("alice", 10)
	if results[0].Name != "Alice" {
		t.Errorf("name = %q, empty upsert should preserve existing name", results[0].Name)
	}
}

func TestFindByExactName(t *testing.T) {
	db := newTestDB(t)
	db.Add("alice@example.com", "Alice Smith", SourceImported)
	db.Add("bob@example.com", "Bob Jones", SourceImported)

	c, err := db.FindByExactName("Alice Smith")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil contact")
	}
	if c.Email != "alice@example.com" {
		t.Errorf("email = %q, want alice", c.Email)
	}
}

func TestFindByExactName_CaseInsensitive(t *testing.T) {
	db := newTestDB(t)
	db.Add("alice@example.com", "Alice Smith", SourceImported)

	c, _ := db.FindByExactName("alice smith")
	if c == nil {
		t.Error("case-insensitive lookup should find contact")
	}
}

func TestFindByExactName_NotFound(t *testing.T) {
	db := newTestDB(t)
	c, err := db.FindByExactName("Nobody")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c != nil {
		t.Error("expected nil for non-existent name")
	}
}

func TestList(t *testing.T) {
	db := newTestDB(t)
	db.Add("alice@example.com", "Alice", SourceImported)
	db.Add("bob@example.com", "Bob", SourceImported)
	db.Add("charlie@example.com", "Charlie", SourceImported)

	results, err := db.List(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("got %d, want 3", len(results))
	}
}

func TestList_RespectsLimit(t *testing.T) {
	db := newTestDB(t)
	db.Add("a@example.com", "A", SourceImported)
	db.Add("b@example.com", "B", SourceImported)
	db.Add("c@example.com", "C", SourceImported)

	results, _ := db.List(2)
	if len(results) != 2 {
		t.Errorf("got %d, want 2", len(results))
	}
}

func TestCount(t *testing.T) {
	db := newTestDB(t)
	db.Add("a@example.com", "A", SourceImported)
	db.Add("b@example.com", "B", SourceImported)

	count, err := db.Count()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestIncrementUsage(t *testing.T) {
	db := newTestDB(t)
	db.Add("alice@example.com", "Alice", SourceImported)

	db.IncrementUsage("alice@example.com")
	db.IncrementUsage("alice@example.com")

	results, _ := db.Search("alice", 10)
	if results[0].UsageCount != 2 {
		t.Errorf("usage = %d, want 2", results[0].UsageCount)
	}
	if results[0].LastUsed.IsZero() {
		t.Error("last_used should be set after increment")
	}
}

func TestDelete(t *testing.T) {
	db := newTestDB(t)
	db.Add("alice@example.com", "Alice", SourceImported)

	if err := db.Delete("alice@example.com"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	count, _ := db.Count()
	if count != 0 {
		t.Errorf("count = %d after delete, want 0", count)
	}
}

func TestDelete_NotFound(t *testing.T) {
	db := newTestDB(t)
	err := db.Delete("nobody@example.com")
	if err == nil {
		t.Error("expected error for non-existent contact")
	}
}

func TestAddBatch(t *testing.T) {
	db := newTestDB(t)
	contacts := []Contact{
		{ID: "1", Email: "a@example.com", Name: "A", Source: SourceImported, CreatedAt: time.Now()},
		{ID: "2", Email: "b@example.com", Name: "B", Source: SourceImported, CreatedAt: time.Now()},
		{ID: "3", Email: "c@example.com", Name: "C", Source: SourceImported, CreatedAt: time.Now()},
	}

	added, _, err := db.AddBatch(contacts)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if added != 3 {
		t.Errorf("added = %d, want 3", added)
	}

	count, _ := db.Count()
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestAddBatch_SkipsInvalidEmail(t *testing.T) {
	db := newTestDB(t)
	contacts := []Contact{
		{ID: "1", Email: "good@example.com", Name: "Good", Source: SourceImported, CreatedAt: time.Now()},
		{ID: "2", Email: "bad-email", Name: "Bad", Source: SourceImported, CreatedAt: time.Now()},
	}

	added, _, _ := db.AddBatch(contacts)
	if added != 1 {
		t.Errorf("added = %d, want 1 (invalid email skipped)", added)
	}
}

func TestCleanInvalid(t *testing.T) {
	db := newTestDB(t)
	// Directly insert an invalid email via raw SQL to bypass validation
	db.db.Exec(`INSERT INTO contacts (id, email, name, source) VALUES ('1', 'bad', 'Bad', 'test')`)
	db.Add("good@example.com", "Good", SourceImported)

	removed, err := db.CleanInvalid()
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	count, _ := db.Count()
	if count != 1 {
		t.Errorf("count = %d, want 1 (only valid contact)", count)
	}
}

func TestSearch_OrderedByUsage(t *testing.T) {
	db := newTestDB(t)
	db.Add("low@example.com", "Low", SourceImported)
	db.Add("high@example.com", "High", SourceImported)

	// Increment high usage
	db.IncrementUsage("high@example.com")
	db.IncrementUsage("high@example.com")

	results, _ := db.List(10)
	if len(results) < 2 {
		t.Fatalf("got %d, want 2+", len(results))
	}
	if results[0].Email != "high@example.com" {
		t.Errorf("first result = %q, want high-usage contact first", results[0].Email)
	}
}

func TestIsValidEmail(t *testing.T) {
	tests := []struct {
		email string
		valid bool
	}{
		{"user@example.com", true},
		{"user.name+tag@example.co.uk", true},
		{"user@sub.domain.com", true},
		{"not-an-email", false},
		{"@example.com", false},
		{"user@", false},
		{"", false},
		{"user@x.y", false},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			if got := isValidEmail(tt.email); got != tt.valid {
				t.Errorf("isValidEmail(%q) = %v, want %v", tt.email, got, tt.valid)
			}
		})
	}
}

func TestFormatDisplay(t *testing.T) {
	c := &Contact{Email: "alice@example.com", Name: "Alice"}
	if got := c.FormatDisplay(); got != "Alice <alice@example.com>" {
		t.Errorf("FormatDisplay() = %q", got)
	}

	c2 := &Contact{Email: "bob@example.com"}
	if got := c2.FormatDisplay(); got != "bob@example.com" {
		t.Errorf("FormatDisplay() = %q, want email only", got)
	}
}
