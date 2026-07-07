package coverage

import (
	"reflect"
	"testing"
)

func TestParseGoProfile(t *testing.T) {
	raw := `mode: set
github.com/org/repo/foo.go:10.20,12.5 2 1
github.com/org/repo/foo.go:12.5,14.2 1 0
github.com/org/repo/foo.go:14.2,16.9 3 1
github.com/org/repo/bar.go:5.1,7.1 1 1
`
	got, err := ParseGoProfile(raw)
	if err != nil {
		t.Fatalf("ParseGoProfile: %v", err)
	}
	if got.Format != FormatGo {
		t.Errorf("Format = %q, want %q", got.Format, FormatGo)
	}
	// foo.go: covered blocks 10-12 and 14-16 (12-14 has count 0). Adjacent? 12
	// and 14 are not adjacent (gap at 13), so two ranges.
	want := []FileCoverage{
		{File: "github.com/org/repo/bar.go", Covered: []LineRange{{5, 7}}},
		{File: "github.com/org/repo/foo.go", Covered: []LineRange{{10, 12}, {14, 16}}},
	}
	if !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("Files = %+v, want %+v", got.Files, want)
	}
}

func TestParseGoProfile_MergesAdjacent(t *testing.T) {
	raw := `mode: count
p/x.go:1.1,2.1 1 5
p/x.go:2.1,3.1 1 2
`
	got, err := ParseGoProfile(raw)
	if err != nil {
		t.Fatalf("ParseGoProfile: %v", err)
	}
	// 1-2 and 2-3 overlap → merge to 1-3.
	want := []FileCoverage{{File: "p/x.go", Covered: []LineRange{{1, 3}}}}
	if !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("Files = %+v, want %+v", got.Files, want)
	}
}

func TestParseGoProfile_Malformed(t *testing.T) {
	if _, err := ParseGoProfile("p/x.go:not-a-range 1 1"); err == nil {
		t.Fatal("expected error for malformed block range")
	}
	if _, err := ParseGoProfile("garbage line here"); err == nil {
		t.Fatal("expected error for malformed line")
	}
}

func TestParseLCOV(t *testing.T) {
	raw := `TN:
SF:src/login.ts
DA:40,1
DA:41,1
DA:42,0
DA:43,5
end_of_record
SF:src/util.ts
DA:1,0
DA:2,3
end_of_record
`
	got, err := ParseLCOV(raw)
	if err != nil {
		t.Fatalf("ParseLCOV: %v", err)
	}
	want := []FileCoverage{
		// login.ts: 40,41 covered (adjacent→40-41), 42 not, 43 covered.
		{File: "src/login.ts", Covered: []LineRange{{40, 41}, {43, 43}}},
		{File: "src/util.ts", Covered: []LineRange{{2, 2}}},
	}
	if !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("Files = %+v, want %+v", got.Files, want)
	}
}

func TestParseDispatch(t *testing.T) {
	if _, err := Parse("go", "mode: set\n"); err != nil {
		t.Errorf("Parse go: %v", err)
	}
	if _, err := Parse("python", "whatever"); err == nil {
		t.Error("expected error for unsupported format")
	}
	if SupportedFormat("python") {
		t.Error("python should not be a supported format")
	}
	if !SupportedFormat("go") || !SupportedFormat("lcov") {
		t.Error("go and lcov should be supported")
	}
}
