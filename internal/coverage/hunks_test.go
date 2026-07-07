package coverage

import (
	"reflect"
	"testing"
)

func TestParseDiffHunks_AddedLines(t *testing.T) {
	diff := `diff --git a/foo.go b/foo.go
index 111..222 100644
--- a/foo.go
+++ b/foo.go
@@ -10,3 +10,5 @@ func Foo() {
 	existing()
+	added1()
+	added2()
 	moreContext()
+	added3()
`
	got := ParseDiffHunks(diff)
	// New-file cursor starts at 10:
	//   line 10 context (existing) → 11
	//   line 11,12 added → run 11-12 → cursor 13
	//   line 13 context (moreContext) → 14
	//   line 14 added → run 14-14
	want := []Hunk{
		{File: "foo.go", Start: 11, End: 12},
		{File: "foo.go", Start: 14, End: 14},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hunks = %+v, want %+v", got, want)
	}
}

func TestParseDiffHunks_NewFile(t *testing.T) {
	diff := `diff --git a/new.go b/new.go
new file mode 100644
index 000..abc
--- /dev/null
+++ b/new.go
@@ -0,0 +1,3 @@
+package p
+
+func New() {}
`
	got := ParseDiffHunks(diff)
	want := []Hunk{{File: "new.go", Start: 1, End: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hunks = %+v, want %+v", got, want)
	}
}

func TestParseDiffHunks_DeletionsProduceNoHunk(t *testing.T) {
	diff := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -5,4 +5,2 @@
 	keep()
-	gone1()
-	gone2()
 	keep2()
`
	got := ParseDiffHunks(diff)
	if len(got) != 0 {
		t.Fatalf("expected no hunks for pure deletion, got %+v", got)
	}
}

func TestParseDiffHunks_MultipleFiles(t *testing.T) {
	diff := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,1 +1,2 @@
 	x()
+	y()
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -20,0 +21,1 @@
+	z()
`
	got := ParseDiffHunks(diff)
	want := []Hunk{
		{File: "a.go", Start: 2, End: 2},
		{File: "b.go", Start: 21, End: 21},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hunks = %+v, want %+v", got, want)
	}
}

func TestParseDiffHunks_Empty(t *testing.T) {
	if got := ParseDiffHunks(""); len(got) != 0 {
		t.Fatalf("expected no hunks for empty diff, got %+v", got)
	}
}
