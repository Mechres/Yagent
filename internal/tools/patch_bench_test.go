package tools

import (
	"fmt"
	"strings"
	"testing"
)

// bigPatch builds a synthetic unified diff: files×hunksPerFile hunks spread
// across files, each with a context line, one removal and one addition.
func bigPatch(files, hunksPerFile int) string {
	var b strings.Builder
	line := 1
	for f := 0; f < files; f++ {
		path := fmt.Sprintf("pkg/file%d.go", f)
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", path, path)
		fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
		for h := 0; h < hunksPerFile; h++ {
			fmt.Fprintf(&b, "@@ -%d,3 +%d,3 @@\n", line, line)
			fmt.Fprintf(&b, " func f%d() int {\n", h)
			fmt.Fprintf(&b, "-    return %d\n", h)
			fmt.Fprintf(&b, "+    return %d\n", h+1)
			fmt.Fprintf(&b, " }\n")
			line += 3
		}
	}
	return b.String()
}

func BenchmarkPatchHunks200(b *testing.B) {
	patch := bigPatch(10, 20) // 200 hunks across 10 files
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hunks, err := PatchHunks(patch)
		if err != nil || len(hunks) != 200 {
			b.Fatalf("hunks = %d / %v", len(hunks), err)
		}
	}
}

func BenchmarkRebuildPatchHalf(b *testing.B) {
	patch := bigPatch(10, 20)
	hunks, err := PatchHunks(patch)
	if err != nil {
		b.Fatal(err)
	}
	keep := make([]bool, len(hunks))
	for i := range keep {
		keep[i] = i%2 == 0
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		filtered, err := RebuildPatch(patch, keep)
		if err != nil || filtered == "" {
			b.Fatalf("rebuild: %v", err)
		}
	}
}

func BenchmarkRebuildPatchKeepOne(b *testing.B) {
	patch := bigPatch(10, 20)
	hunks, err := PatchHunks(patch)
	if err != nil {
		b.Fatal(err)
	}
	keep := make([]bool, len(hunks))
	keep[0] = true
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		filtered, err := RebuildPatch(patch, keep)
		if err != nil || filtered == "" {
			b.Fatalf("rebuild: %v", err)
		}
	}
}
