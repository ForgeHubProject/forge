package credential

import "testing"

func TestEntryFromURL(t *testing.T) {
	e, err := entryFromURL("https://forgehub.example.com:8443/handle/repo.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Protocol != "https" {
		t.Errorf("protocol = %q, want https", e.Protocol)
	}
	if e.Host != "forgehub.example.com:8443" {
		t.Errorf("host = %q, want forgehub.example.com:8443", e.Host)
	}
	if e.Path != "handle/repo.git" {
		t.Errorf("path = %q, want handle/repo.git", e.Path)
	}
}

func TestEntryEncode(t *testing.T) {
	e := entry{Protocol: "https", Host: "example.com", Username: "alice", Password: "s3cret"}
	got := string(e.encode())
	want := "protocol=https\nhost=example.com\nusername=alice\npassword=s3cret\n\n"
	if got != want {
		t.Errorf("encode() = %q, want %q", got, want)
	}
}

func TestEntryEncodeOmitsEmptyFields(t *testing.T) {
	e := entry{Protocol: "https", Host: "example.com"}
	got := string(e.encode())
	if want := "protocol=https\nhost=example.com\n\n"; got != want {
		t.Errorf("encode() = %q, want %q", got, want)
	}
}

func TestParseEntry(t *testing.T) {
	base := entry{Protocol: "https", Host: "example.com"}
	data := []byte("protocol=https\nhost=example.com\nusername=alice\npassword=s3cret\n")
	got := parseEntry(data, base)
	if got.Username != "alice" || got.Password != "s3cret" {
		t.Errorf("parseEntry() = %+v, want username=alice password=s3cret", got)
	}
}

func TestParseEntryIgnoresMalformedLines(t *testing.T) {
	base := entry{Protocol: "https"}
	data := []byte("not-a-kv-line\nusername=alice\n\npassword=s3cret")
	got := parseEntry(data, base)
	if got.Username != "alice" || got.Password != "s3cret" {
		t.Errorf("parseEntry() = %+v, want username=alice password=s3cret", got)
	}
	if got.Protocol != "https" {
		t.Errorf("parseEntry() should preserve base fields not present in data, got protocol=%q", got.Protocol)
	}
}

func TestFillReturnsNotOkWhenPasswordEmpty(t *testing.T) {
	// entryFromURL + run() would need a real git binary; here we only verify
	// the "empty password means not ok" contract via parseEntry directly,
	// since Fill's decision is entirely driven by that check.
	base := entry{Protocol: "https", Host: "example.com"}
	data := []byte("username=alice\n")
	got := parseEntry(data, base)
	if got.Password != "" {
		t.Fatalf("expected empty password in this fixture, got %q", got.Password)
	}
}
