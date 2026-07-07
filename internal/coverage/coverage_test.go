package coverage

import "testing"

func TestLineRangeOverlaps(t *testing.T) {
	tests := []struct {
		name string
		a, b LineRange
		want bool
	}{
		{"disjoint before", LineRange{1, 5}, LineRange{6, 10}, false},
		{"disjoint after", LineRange{20, 30}, LineRange{6, 10}, false},
		{"touching edge", LineRange{1, 6}, LineRange{6, 10}, true},
		{"contained", LineRange{5, 8}, LineRange{1, 20}, true},
		{"partial", LineRange{1, 7}, LineRange{5, 10}, true},
		{"identical", LineRange{3, 3}, LineRange{3, 3}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Overlaps(tt.b); got != tt.want {
				t.Fatalf("Overlaps = %v, want %v", got, tt.want)
			}
			// Overlap is symmetric.
			if got := tt.b.Overlaps(tt.a); got != tt.want {
				t.Fatalf("symmetric Overlaps = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHunkCovered(t *testing.T) {
	datasets := []CoverageData{{
		Format: FormatGo,
		Files: []FileCoverage{
			{File: "github.com/org/repo/internal/foo.go", Covered: []LineRange{{10, 15}, {40, 42}}},
		},
	}}
	tests := []struct {
		name string
		hunk Hunk
		want bool
	}{
		{"covered exact", Hunk{"internal/foo.go", 10, 15}, true},
		{"covered overlap edge", Hunk{"internal/foo.go", 15, 20}, true},
		{"covered inside", Hunk{"internal/foo.go", 41, 41}, true},
		{"uncovered gap", Hunk{"internal/foo.go", 20, 30}, false},
		{"uncovered other file", Hunk{"internal/bar.go", 10, 15}, false},
		{"suffix match with module prefix", Hunk{"repo/internal/foo.go", 12, 12}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HunkCovered(tt.hunk, datasets); got != tt.want {
				t.Fatalf("HunkCovered(%v) = %v, want %v", tt.hunk, got, tt.want)
			}
		})
	}
}

func TestFileMatch(t *testing.T) {
	tests := []struct {
		cov, hunk string
		want      bool
	}{
		{"github.com/org/repo/internal/foo.go", "internal/foo.go", true},
		{"internal/foo.go", "github.com/org/repo/internal/foo.go", true},
		{"internal/foo.go", "internal/foo.go", true},
		{"/abs/path/internal/foo.go", "internal/foo.go", true},
		{"internal/foobar.go", "internal/bar.go", false},
		{"internal/foo.go", "internal/other/foo.go", false}, // diverge at the directory segment
		{"a/b/foo.go", "x/y/foo.go", false},                 // differ at second-to-last segment
		{"", "internal/foo.go", false},
	}
	for _, tt := range tests {
		if got := fileMatch(tt.cov, tt.hunk); got != tt.want {
			t.Errorf("fileMatch(%q,%q) = %v, want %v", tt.cov, tt.hunk, got, tt.want)
		}
	}
}

func TestValidState(t *testing.T) {
	for _, s := range []string{StateRuntimeVerified, StateStaticVerified, StateAttested, StateUnverified} {
		if !ValidState(s) {
			t.Errorf("ValidState(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "verified", "runtime", "RUNTIME-VERIFIED"} {
		if ValidState(s) {
			t.Errorf("ValidState(%q) = true, want false", s)
		}
	}
}
