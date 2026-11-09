package orchestrator

import "testing"

func TestSplitCompleteLogLinesCarriesPartialLineAcrossChunks(t *testing.T) {
	complete, partial := splitCompleteLogLines("old ", "line\nsecond", " line\nthird")
	if complete != "old line\nsecond line" || partial != "third" {
		t.Fatalf("complete=%q partial=%q", complete, partial)
	}
}

func TestSplitCompleteLogLinesSupportsRotationChunksWithoutDuplication(t *testing.T) {
	complete, partial := splitCompleteLogLines("", "rotated-1\nrotated-2\n", "current-1\ncurrent-part")
	if complete != "rotated-1\nrotated-2\ncurrent-1" || partial != "current-part" {
		t.Fatalf("complete=%q partial=%q", complete, partial)
	}
}
