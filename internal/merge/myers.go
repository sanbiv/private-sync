package merge

import "math"

// Myers O(ND) diff (E. Myers, "An O(ND) Difference Algorithm and Its
// Variations", 1986) in its linear-space "middle snake" form. Sequences are
// slices of interned line ids; the output is a monotone matching a→b.

// myersMinLimit is the minimum number of Myers iterations allowed per
// middle-snake search. The effective limit grows with the input size (see
// myersLimit); beyond it the search splits at the furthest reaching point so
// that wildly different inputs stay fast at the cost of a possibly
// non-minimal diff (the same trade-off git's xdiff makes).
const myersMinLimit = 1024

// myersLimit returns the iteration cap for a sub-problem of total size s.
func myersLimit(s int) int {
	if l := 4 * int(math.Sqrt(float64(s))); l > myersMinLimit {
		return l
	}
	return myersMinLimit
}

// myersMatch returns, for each index of a, the index of the matched element
// in b, or -1 when unmatched. The matching is monotone and one-to-one.
func myersMatch(a, b []int) []int {
	ma := make([]int, len(a))
	for i := range ma {
		ma[i] = -1
	}
	if len(a) == 0 || len(b) == 0 {
		return ma
	}
	max := (len(a)+len(b)+1)/2 + 1
	d := &myers{
		a:  a,
		b:  b,
		ma: ma,
		vf: make([]int, 2*max+3),
		vb: make([]int, 2*max+3),
	}
	d.run(0, len(a), 0, len(b))
	return ma
}

type myers struct {
	a, b   []int
	ma     []int
	vf, vb []int // scratch V arrays, reused across recursion levels
}

// run matches a[a0:a1] against b[b0:b1].
func (d *myers) run(a0, a1, b0, b1 int) {
	for {
		// Common prefix.
		for a0 < a1 && b0 < b1 && d.a[a0] == d.b[b0] {
			d.ma[a0] = b0
			a0++
			b0++
		}
		// Common suffix.
		for a0 < a1 && b0 < b1 && d.a[a1-1] == d.b[b1-1] {
			a1--
			b1--
			d.ma[a1] = b1
		}
		if a0 >= a1 || b0 >= b1 {
			return
		}
		x1, y1, x2, y2, ok := d.middle(a0, a1, b0, b1)
		if !ok {
			return // treated as delete-all/insert-all
		}
		for i := 0; i < x2-x1; i++ {
			d.ma[a0+x1+i] = b0 + y1 + i
		}
		// Recurse on the smaller half, iterate on the larger one.
		left := (x1) + (y1)
		right := (a1 - a0 - x2) + (b1 - b0 - y2)
		if left <= right {
			d.run(a0, a0+x1, b0, b0+y1)
			a0, b0 = a0+x2, b0+y2
		} else {
			d.run(a0+x2, a1, b0+y2, b1)
			a1, b1 = a0+x1, b0+y1
		}
	}
}

// middle finds a middle snake of a[a0:a1] vs b[b0:b1]. It returns the snake
// (x1,y1)→(x2,y2) relative to (a0,b0); the snake may be empty, in which case
// the point is still a valid split. ok is false when no split was found.
func (d *myers) middle(a0, a1, b0, b1 int) (x1, y1, x2, y2 int, ok bool) {
	a, b := d.a[a0:a1], d.b[b0:b1]
	n, m := len(a), len(b)
	delta := n - m
	odd := delta&1 != 0
	max := (n + m + 1) / 2
	off := max + 1
	vf, vb := d.vf, d.vb
	for i := 0; i <= 2*max+2; i++ {
		vf[i] = -1
		vb[i] = -1
	}
	limit := myersLimit(n + m)
	for dd := 0; dd <= max; dd++ {
		if dd > limit {
			return d.heuristicSplit(n, m, dd-1, off)
		}
		// Forward paths from (0,0).
		for k := -dd; k <= dd; k += 2 {
			x := -1
			if dd == 0 {
				x = 0
			} else {
				if k > -dd {
					if v := vf[off+k-1]; v >= 0 && v < n {
						x = v + 1 // right move
					}
				}
				if k < dd {
					if v := vf[off+k+1]; v >= 0 && v-(k+1) < m && v > x {
						x = v // down move
					}
				}
			}
			if x < 0 {
				vf[off+k] = -1
				continue
			}
			y := x - k
			sx, sy := x, y
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			vf[off+k] = x
			if odd && k >= delta-(dd-1) && k <= delta+(dd-1) {
				if xr := vb[off+delta-k]; xr >= 0 && x+xr >= n {
					return sx, sy, x, y, true
				}
			}
		}
		// Reverse paths from (n,m), computed as forward paths on the reversed
		// sequences; reverse diagonal kr maps to forward diagonal delta-kr.
		for k := -dd; k <= dd; k += 2 {
			x := -1
			if dd == 0 {
				x = 0
			} else {
				if k > -dd {
					if v := vb[off+k-1]; v >= 0 && v < n {
						x = v + 1
					}
				}
				if k < dd {
					if v := vb[off+k+1]; v >= 0 && v-(k+1) < m && v > x {
						x = v
					}
				}
			}
			if x < 0 {
				vb[off+k] = -1
				continue
			}
			y := x - k
			sx, sy := x, y
			for x < n && y < m && a[n-1-x] == b[m-1-y] {
				x++
				y++
			}
			vb[off+k] = x
			if !odd && delta-k >= -dd && delta-k <= dd {
				if xf := vf[off+delta-k]; xf >= 0 && x+xf >= n {
					return n - x, m - y, n - sx, m - sy, true
				}
			}
		}
	}
	return 0, 0, 0, 0, false
}

// heuristicSplit picks the furthest reaching forward point after dd
// iterations as an (empty-snake) split point.
func (d *myers) heuristicSplit(n, m, dd, off int) (int, int, int, int, bool) {
	bestX, bestY, best := -1, -1, -1
	for k := -dd; k <= dd; k += 2 {
		x := d.vf[off+k]
		if x < 0 {
			continue
		}
		y := x - k
		if x > n || y < 0 || y > m {
			continue
		}
		if x+y > best {
			bestX, bestY, best = x, y, x+y
		}
	}
	if best <= 0 || best >= n+m {
		return 0, 0, 0, 0, false
	}
	return bestX, bestY, bestX, bestY, true
}
