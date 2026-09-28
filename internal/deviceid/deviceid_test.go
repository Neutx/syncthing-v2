package deviceid

import (
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Vectors after upstream Syncthing's lib/protocol deviceid_test.go: every
// entry of formatCases denotes the same device, whose canonical string is
// formatted. The 52-character entries are the canonical string with its four
// Luhn characters (Y, I, M, 2) removed.
const formatted = "P56IOI7-MZJNU2Y-IQGDREY-DM2MGTI-MGL3BXN-PQ6W5BM-TBBZ4TJ-XZWICQ2"

var formatCases = []string{
	"P56IOI-7MZJNU-2IQGDR-EYDM2M-GTMGL3-BXNPQ6-W5BTBB-Z4TJXZ-WICQ",
	"P56IOI-7MZJNU2Y-IQGDR-EYDM2M-GTI-MGL3-BXNPQ6-W5BM-TBB-Z4TJ-XZWICQ2",
	"P56IOI7 MZJNU2I QGDREYD M2MGTMGL 3BXNPQ6W 5BTBB Z4TJXZWICQ",
	"P56IOI7 MZJNU2Y IQGDREY DM2MGTI MGL3BXN PQ6W5BM TBBZ4TJ XZWICQ2",
	"P56IOI7MZJNU2IQGDREYDM2MGTMGL3BXNPQ6W5BTBBZ4TJXZWICQ",
	"p56ioi7mzjnu2iqgdreydm2mgtmgl3bxnpq6w5btbbz4tjxzwicq",
	"P56IOI7MZJNU2YIQGDREYDM2MGTIMGL3BXNPQ6W5BMTBBZ4TJXZWICQ2",
	"P561017MZJNU2YIQGDREYDM2MGTIMGL3BXNPQ6W5BMTBBZ4TJXZWICQ2",
	"p56ioi7mzjnu2yiqgdreydm2mgtimgl3bxnpq6w5bmtbbz4tjxzwicq2",
	"p561017mzjnu2yiqgdreydm2mgtimgl3bxnpq6w5bmtbbz4tjxzwicq2",
}

func TestParseUpstreamVectors(t *testing.T) {
	for _, in := range formatCases {
		got, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if got != formatted {
			t.Errorf("Parse(%q) = %q, want %q", in, got, formatted)
		}
	}
}

func TestLuhn32UpstreamVector(t *testing.T) {
	// upstream lib/protocol luhn_test.go
	if got := luhn32("AB725E4GHIQPL3ZFGT"); got != 'G' {
		t.Fatalf("luhn32 = %q, want 'G'", got)
	}
	// The four check characters of the formatted vector.
	raw := strings.ReplaceAll(formatted, "-", "")
	for i, want := range []byte{raw[13], raw[27], raw[41], raw[55]} {
		g := raw[i*14 : i*14+13]
		if got := luhn32(g); got != want {
			t.Errorf("group %d: luhn32(%q) = %q, want %q", i, g, got, want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"",
		"P56IOI7-MZJNU2Y",
		// Wrong check character in the first group (Y → Z).
		"P56IOI7-MZJNU2Z-IQGDREY-DM2MGTI-MGL3BXN-PQ6W5BM-TBBZ4TJ-XZWICQ2",
		// '9' is not base32 and has no look-alike mapping.
		"P56IOI7-MZJNU2Y-IQGDREY-DM2MGTI-MGL3BXN-PQ6W5BM-TBBZ4TJ-XZWIC92",
		"P56IOI7MZJNU2IQGDREYDM2MGTMGL3BXNPQ6W5BTBBZ4TJXZWIC9",
		strings.Repeat("A", 60),
	}
	for _, in := range bad {
		if got, err := Parse(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %q, %v; want ErrInvalid", in, got, err)
		}
	}
}

func TestValid(t *testing.T) {
	if !Valid(formatted) {
		t.Errorf("Valid(%q) = false", formatted)
	}
	for _, in := range []string{formatCases[0], strings.ToLower(formatted), "short"} {
		if Valid(in) {
			t.Errorf("Valid(%q) = true for a non-canonical string", in)
		}
	}
}

// synthetic-cert.pem is a throwaway self-signed certificate generated for
// this test (its key was discarded). synthetic-cert.id was produced by
// running upstream `syncthing device-id` against it.
func TestFromCertCommittedFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "synthetic-cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(data)
	if blk == nil || blk.Type != "CERTIFICATE" {
		t.Fatal("testdata/synthetic-cert.pem holds no CERTIFICATE block")
	}
	want, err := os.ReadFile(filepath.Join("testdata", "synthetic-cert.id"))
	if err != nil {
		t.Fatal(err)
	}
	got := FromCert(blk.Bytes)
	if got != strings.TrimSpace(string(want)) {
		t.Fatalf("FromCert = %q, want %q", got, strings.TrimSpace(string(want)))
	}
	if !Valid(got) {
		t.Fatalf("FromCert result %q is not canonical", got)
	}
	if len(got) != 63 || strings.Count(got, "-") != 7 {
		t.Fatalf("FromCert = %q: want 8 dash-separated groups of 7", got)
	}
}

func TestFromCertDiffers(t *testing.T) {
	a, b := FromCert([]byte{1}), FromCert([]byte{2})
	if a == b {
		t.Fatal("different certificates produced the same ID")
	}
	if FromCert([]byte{1}) != a {
		t.Fatal("FromCert is not deterministic")
	}
}

func TestShort(t *testing.T) {
	if got := Short(formatted); got != "P56IOI7" {
		t.Errorf("Short = %q", got)
	}
	if got := Short("ABC"); got != "ABC" {
		t.Errorf("Short(short) = %q", got)
	}
}
