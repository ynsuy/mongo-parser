package source

import (
	"strings"
	"testing"
)

func TestMissingSplitPiecesAreNamed(t *testing.T) {
	got := missingPieces([]string{"x.gz_part_ab", "x.gz_part_ac", "x.gz_part_af", "x.gz_part_ag"})
	if strings.Join(got, ",") != "aa,ad,ae" {
		t.Fatalf("missing %v", got)
	}
}
