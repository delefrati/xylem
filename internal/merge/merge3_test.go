package merge

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestMerge3(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		base, ours, theirs string
		want               string
		regions            []Region
	}{
		{
			name: "disjoint edits",
			base: "1\n2\n3\n4\n5\n", ours: "one\n2\n3\n4\n5\n", theirs: "1\n2\n3\n4\nfive\n",
			want: "one\n2\n3\n4\nfive\n",
		},
		{
			name: "insert and delete in different places",
			base: "a\nb\nc\nd\n", ours: "a\nnew\nb\nc\nd\n", theirs: "a\nb\nc\n",
			want: "a\nnew\nb\nc\n",
		},
		{
			name: "only ours changed",
			base: "a\nb\n", ours: "a\nB\n", theirs: "a\nb\n", want: "a\nB\n",
		},
		{
			name: "only theirs changed",
			base: "a\nb\n", ours: "a\nb\n", theirs: "x\nb\n", want: "x\nb\n",
		},
		{
			name: "identical change",
			base: "a\nb\nc\n", ours: "a\nB\nc\n", theirs: "a\nB\nc\n", want: "a\nB\nc\n",
		},
		{
			name: "no final newline kept",
			base: "a\nx\nb", ours: "A\nx\nb", theirs: "a\nx\nB", want: "A\nx\nB",
		},
		{
			name: "same line changed differently",
			base: "a\nb\nc\n", ours: "a\nours\nc\n", theirs: "a\ntheirs\nc\n",
			want:    "a\n" + MarkerOurs + "ours\n" + MarkerSep + "theirs\n" + MarkerTheirs + "c\n",
			regions: []Region{{1, 2, 1, 2, 1, 2}},
		},
		{
			name: "adjacent changes conflict",
			base: "a\nb\nc\n", ours: "A\nb\nc\n", theirs: "a\nB\nc\n",
			want:    MarkerOurs + "A\nb\n" + MarkerSep + "a\nB\n" + MarkerTheirs + "c\n",
			regions: []Region{{0, 2, 0, 2, 0, 2}},
		},
		{
			name: "insert at the same place",
			base: "a\nb\n", ours: "a\nx\nb\n", theirs: "a\ny\nb\n",
			want:    "a\n" + MarkerOurs + "x\n" + MarkerSep + "y\n" + MarkerTheirs + "b\n",
			regions: []Region{{1, 1, 1, 2, 1, 2}},
		},
		{
			name: "add/add with an empty base, same content",
			base: "", ours: "x\ny\n", theirs: "x\ny\n", want: "x\ny\n",
		},
		{
			name: "add/add with an empty base, different content",
			base: "", ours: "x\n", theirs: "y\n",
			want:    MarkerOurs + "x\n" + MarkerSep + "y\n" + MarkerTheirs,
			regions: []Region{{0, 0, 0, 1, 0, 1}},
		},
		{
			name: "delete vs edit of the same line",
			base: "a\nb\nc\n", ours: "a\nc\n", theirs: "a\nB\nc\n",
			want:    "a\n" + MarkerOurs + MarkerSep + "B\n" + MarkerTheirs + "c\n",
			regions: []Region{{1, 2, 1, 1, 1, 2}},
		},
		{
			name: "conflict marker gets a newline",
			base: "a\nb", ours: "a\nx", theirs: "a\ny",
			want:    "a\n" + MarkerOurs + "x\n" + MarkerSep + "y\n" + MarkerTheirs,
			regions: []Region{{1, 2, 1, 2, 1, 2}},
		},
		{
			name: "offsets in later regions",
			base: "1\n2\n3\n4\n5\n6\n7\n", ours: "0\n1\n2\n3\n4\nfive\n6\n7\n", theirs: "1\n2\n3\n4\nFIVE\n6\n7\n",
			want:    "0\n1\n2\n3\n4\n" + MarkerOurs + "five\n" + MarkerSep + "FIVE\n" + MarkerTheirs + "6\n7\n",
			regions: []Region{{4, 5, 5, 6, 4, 5}},
		},
	} {
		got, regions, err := Merge3([]byte(tc.base), []byte(tc.ours), []byte(tc.theirs))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: merged\n%q\nwant\n%q", tc.name, got, tc.want)
		}
		if diff := cmp.Diff(tc.regions, regions); diff != "" {
			t.Errorf("%s: regions (-want +got):\n%s", tc.name, diff)
		}
	}
	if _, _, err := Merge3([]byte("a\n"), []byte("a\x00\n"), []byte("b\n")); !errors.Is(err, ErrBinary) {
		t.Errorf("binary: %v", err)
	}
}
