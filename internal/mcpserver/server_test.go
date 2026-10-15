package mcpserver

import (
	"slices"
	"strings"
	"testing"
)

func TestPaginateKeepsEveryByteAndWholeCharacters(t *testing.T) {
	text := strings.Repeat("ağ€x", 1000)
	pages := paginate(text, 4096)
	if len(pages) < 2 || strings.Join(pages, "") != text {
		t.Fatalf("%d pages do not join back to the text", len(pages))
	}
	for _, page := range pages {
		if len(page) > 4096 || !strings.HasPrefix(text[strings.Index(text, page):], page) {
			t.Fatalf("page of %d bytes", len(page))
		}
	}
}

func TestFlagsLeaveOutEmptyValues(t *testing.T) {
	got := flags("from", "", "top", 0, "chart", false, "source", []string{"a", "b"}, "bin", 60, "newest", true)
	want := []string{"--source", "a", "--source", "b", "--bin", "60", "--newest"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
}
