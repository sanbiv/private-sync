package merge

import (
	"math/rand"
	"testing"
)

// lcsLen is the O(n*m) reference used only to check optimality on small inputs.
func lcsLen(a, b []int) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] >= cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// checkMatching verifies that ma is a monotone one-to-one matching of equal
// elements and returns the number of matched pairs.
func checkMatching(t *testing.T, a, b, ma []int) int {
	t.Helper()
	if len(ma) != len(a) {
		t.Fatalf("matching length %d, want %d", len(ma), len(a))
	}
	last := -1
	n := 0
	for i, j := range ma {
		if j < 0 {
			continue
		}
		if j >= len(b) {
			t.Fatalf("ma[%d]=%d out of range (len(b)=%d)", i, j, len(b))
		}
		if j <= last {
			t.Fatalf("matching not monotone at a[%d]→b[%d] (previous %d)", i, j, last)
		}
		if a[i] != b[j] {
			t.Fatalf("matched unequal elements a[%d]=%d b[%d]=%d", i, a[i], j, b[j])
		}
		last = j
		n++
	}
	return n
}

func TestMyersMatchEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		a, b []int
		want []int
	}{
		{"both empty", nil, nil, []int{}},
		{"a empty", nil, []int{1, 2}, []int{}},
		{"b empty", []int{1, 2}, nil, []int{-1, -1}},
		{"identical", []int{1, 2, 3}, []int{1, 2, 3}, []int{0, 1, 2}},
		{"disjoint", []int{1, 2}, []int{3, 4}, []int{-1, -1}},
		{"insert middle", []int{1, 3}, []int{1, 2, 3}, []int{0, 2}},
		{"delete middle", []int{1, 2, 3}, []int{1, 3}, []int{0, -1, 1}},
		{"replace", []int{1, 2, 3}, []int{1, 9, 3}, []int{0, -1, 2}},
		{"prefix only", []int{1, 2}, []int{1, 5, 6}, []int{0, -1}},
		{"suffix only", []int{7, 2}, []int{5, 6, 2}, []int{-1, 2}},
		{"repeated", []int{1, 1, 1}, []int{1, 1}, nil}, // any two of the three
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := myersMatch(tc.a, tc.b)
			n := checkMatching(t, tc.a, tc.b, got)
			if want := lcsLen(tc.a, tc.b); n != want {
				t.Fatalf("matched %d pairs, LCS is %d (got %v)", n, want, got)
			}
			if tc.want != nil && !eqInts(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMyersMatchRandomIsOptimal(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 3000; iter++ {
		alpha := 1 + rng.Intn(6)
		a := make([]int, rng.Intn(25))
		b := make([]int, rng.Intn(25))
		for i := range a {
			a[i] = rng.Intn(alpha)
		}
		for i := range b {
			b[i] = rng.Intn(alpha)
		}
		got := myersMatch(a, b)
		n := checkMatching(t, a, b, got)
		if want := lcsLen(a, b); n != want {
			t.Fatalf("iter %d: a=%v b=%v matched %d, LCS %d, ma=%v", iter, a, b, n, want, got)
		}
	}
}

func TestMyersMatchLargeShapes(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	// Long common sequence with scattered edits.
	a := make([]int, 5000)
	for i := range a {
		a[i] = i
	}
	b := make([]int, 0, 5200)
	for i, v := range a {
		if i%97 == 0 {
			continue // deletion
		}
		b = append(b, v)
		if i%131 == 0 {
			b = append(b, 100000+i) // insertion
		}
	}
	got := myersMatch(a, b)
	n := checkMatching(t, a, b, got)
	if want := lcsLen(a, b); n != want {
		t.Fatalf("matched %d, LCS %d", n, want)
	}
	// Completely different large inputs exercise the cost limit.
	c := make([]int, 20000)
	d := make([]int, 20000)
	for i := range c {
		c[i] = rng.Intn(1 << 30)
		d[i] = -1 - rng.Intn(1<<30)
	}
	got = myersMatch(c, d)
	if n := checkMatching(t, c, d, got); n != 0 {
		t.Fatalf("disjoint inputs matched %d pairs", n)
	}
}
