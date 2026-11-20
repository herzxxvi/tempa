package gitobj

// History algorithms. They mark visited commits with an epoch stamp instead of building sets, so a
// walk allocates nothing once the scratch stack has grown. Callbacks passed to Walk must not call
// back into these functions.

func (db *DB) nextEpoch() uint32 {
	db.epoch++
	if db.epoch == 0 { // wrapped: clear every stamp so stale ones cannot match
		for _, o := range db.objs {
			if o.Commit != nil {
				o.Commit.seen = 0
			}
		}
		db.epoch = 1
	}
	return db.epoch
}

// gen returns the generation number of a commit: 1 for a root (or a commit whose parents are not
// stored), otherwise 1 + the largest generation among its parents. An ancestor always has a smaller
// generation than its descendants, which lets walks stop early.
func (db *DB) gen(o *Object) uint32 {
	if g := o.Commit.gen; g != 0 {
		return g
	}
	var local [16]*Object
	stack := append(local[:0], o)
	for len(stack) > 0 {
		top := stack[len(stack)-1].Commit
		if top.gen != 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		g, ready := uint32(0), true
		for _, p := range top.Parents {
			po := db.GetCommit(p)
			if po == nil {
				continue
			}
			if pg := po.Commit.gen; pg == 0 {
				stack = append(stack, po)
				ready = false
			} else if pg > g {
				g = pg
			}
		}
		if ready {
			top.gen = g + 1
			stack = stack[:len(stack)-1]
		}
	}
	return o.Commit.gen
}

// IsAncestor reports whether anc is desc itself or reachable from desc through parent links. The
// usual push (the new tip's parent is the old tip) is answered without a walk.
func (db *DB) IsAncestor(anc, desc OID) bool {
	d := db.GetCommit(desc)
	if d == nil {
		return false
	}
	if anc == desc {
		return true
	}
	for _, p := range d.Commit.Parents {
		if p == anc {
			return true
		}
	}
	a := db.GetCommit(anc)
	if a == nil {
		return false
	}
	floor := db.gen(a)
	if db.gen(d) <= floor {
		return false
	}
	epoch := db.nextEpoch()
	stack := append(db.stack[:0], d)
	found := false
walk:
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range c.Commit.Parents {
			if p == anc {
				found = true
				break walk
			}
			po := db.GetCommit(p)
			// Nothing at or below anc's generation can still reach anc.
			if po == nil || po.Commit.seen == epoch || db.gen(po) <= floor {
				continue
			}
			po.Commit.seen = epoch
			stack = append(stack, po)
		}
	}
	db.stack = stack[:0]
	return found
}

// markAncestors stamps o and everything reachable from it with epoch.
func (db *DB) markAncestors(o *Object, epoch uint32) {
	o.Commit.seen = epoch
	stack := append(db.stack[:0], o)
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range c.Commit.Parents {
			if po := db.GetCommit(p); po != nil && po.Commit.seen != epoch {
				po.Commit.seen = epoch
				stack = append(stack, po)
			}
		}
	}
	db.stack = stack[:0]
}

// MergeBase returns a best common ancestor of a and b: a common ancestor that is not an ancestor of
// any other common ancestor. ok is false when the histories are unrelated or a commit is missing.
// When several exist the one found first walking b's parents in order wins, which is deterministic.
func (db *DB) MergeBase(a, b OID) (OID, bool) {
	ao, bo := db.GetCommit(a), db.GetCommit(b)
	if ao == nil || bo == nil {
		return Zero, false
	}
	if db.IsAncestor(a, b) {
		return a, true
	}
	if db.IsAncestor(b, a) {
		return b, true
	}
	inA := db.nextEpoch()
	db.markAncestors(ao, inA)
	inB := db.nextEpoch()
	var best *Object
	bo.Commit.seen = inB
	queue := append(db.stack[:0], bo)
	for head := 0; head < len(queue); head++ {
		c := queue[head]
		for _, p := range c.Commit.Parents {
			po := db.GetCommit(p)
			if po == nil || po.Commit.seen == inB {
				continue
			}
			if po.Commit.seen == inA {
				// Common ancestor: a candidate, and nothing beyond it can be better.
				if best == nil || db.gen(po) > db.gen(best) {
					best = po
				}
				continue
			}
			po.Commit.seen = inB
			queue = append(queue, po)
		}
	}
	db.stack = queue[:0]
	if best == nil {
		return Zero, false
	}
	return best.ID, true
}

// Walk calls fn for tip and its ancestors, newest committer time first (ties: the commit reached
// first), each commit once, until fn returns false. This is the order of a commit listing.
func (db *DB) Walk(tip OID, fn func(o *Object) bool) {
	start := db.GetCommit(tip)
	if start == nil {
		return
	}
	epoch := db.nextEpoch()
	start.Commit.seen = epoch
	front := append(db.stack[:0], start)
	for len(front) > 0 {
		best := 0
		for i := 1; i < len(front); i++ {
			if front[i].Commit.Time > front[best].Commit.Time {
				best = i
			}
		}
		c := front[best]
		front = append(front[:best], front[best+1:]...)
		if !fn(c) {
			break
		}
		for _, p := range c.Commit.Parents {
			if po := db.GetCommit(p); po != nil && po.Commit.seen != epoch {
				po.Commit.seen = epoch
				front = append(front, po)
			}
		}
	}
	db.stack = front[:0]
}

// CountAhead returns how many commits are reachable from tip but not from base. A Zero or unknown
// base counts the whole history of tip.
func (db *DB) CountAhead(tip, base OID) int {
	t := db.GetCommit(tip)
	if t == nil {
		return 0
	}
	var inBase uint32
	if bo := db.GetCommit(base); bo != nil {
		inBase = db.nextEpoch()
		db.markAncestors(bo, inBase)
		if t.Commit.seen == inBase {
			return 0
		}
	}
	epoch := db.nextEpoch()
	t.Commit.seen = epoch
	stack := append(db.stack[:0], t)
	n := 0
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n++
		for _, p := range c.Commit.Parents {
			po := db.GetCommit(p)
			if po == nil || po.Commit.seen == epoch || (inBase != 0 && po.Commit.seen == inBase) {
				continue
			}
			po.Commit.seen = epoch
			stack = append(stack, po)
		}
	}
	db.stack = stack[:0]
	return n
}
