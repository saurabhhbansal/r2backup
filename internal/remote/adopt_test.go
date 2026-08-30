package remote

import "testing"

// Which of the server's parts may be kept when resuming.
//
// This is the rule that decides whether an interrupted upload can finish at
// all. A part that is short is not merely useless: S3 refuses to complete an
// upload whose non-final parts are under its 5MiB floor, so keeping one makes
// every future attempt fail at the last step with the same complaint, and no
// amount of resuming ever fixes it.
func TestOnlyRightSizedPartsAreAdopted(t *testing.T) {
	const partSize = 5 << 20
	const size = 12 << 20 // parts of 5MiB, 5MiB, 2MiB
	const total = 3

	cases := []struct {
		name string
		in   []PartRecord
		want []int32
	}{
		{
			"all three the size they should be",
			[]PartRecord{{1, "a", 5 << 20}, {2, "b", 5 << 20}, {3, "c", 2 << 20}},
			[]int32{1, 2, 3},
		},
		{
			"a body part cut short is dropped and re-sent",
			[]PartRecord{{1, "a", 1 << 20}, {2, "b", 5 << 20}},
			[]int32{2},
		},
		{
			"the last part is the remainder, not a full part",
			[]PartRecord{{3, "c", 2 << 20}},
			[]int32{3},
		},
		{
			"a last part of full size is wrong for this file",
			[]PartRecord{{3, "c", 5 << 20}},
			nil,
		},
		{
			"a part number past the end of the file is ignored",
			[]PartRecord{{4, "d", 5 << 20}},
			nil,
		},
		{
			"part zero is not a part",
			[]PartRecord{{0, "z", 5 << 20}},
			nil,
		},
		{"nothing on the server", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := adoptable(tc.in, total, partSize, size)
			if len(got) != len(tc.want) {
				t.Fatalf("adopted %d parts %v, want %v", len(got), keysOf(got), tc.want)
			}
			for _, n := range tc.want {
				if _, ok := got[n]; !ok {
					t.Errorf("part %d should have been adopted; got %v", n, keysOf(got))
				}
			}
		})
	}
}

func keysOf(m map[int32]PartRecord) []int32 {
	out := make([]int32, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	return out
}

// A file that divides exactly into parts has no remainder, and its last part
// is a full one. Getting this wrong would drop the final part of every
// evenly-sized file and re-upload it on every resume.
func TestAFileThatDividesExactlyHasAFullLastPart(t *testing.T) {
	const partSize = 5 << 20
	const size = 10 << 20
	const total = 2
	got := adoptable([]PartRecord{{1, "a", 5 << 20}, {2, "b", 5 << 20}}, total, partSize, size)
	if len(got) != 2 {
		t.Fatalf("adopted %v, want both parts", keysOf(got))
	}
}
