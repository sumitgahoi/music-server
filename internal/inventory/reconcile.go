package inventory

import "sort"

// locationRow is one stored path for an asset.
type locationRow struct {
	id      int64
	path    string
	present bool
}

// relocation points an existing location row at a new path.
type relocation struct {
	id   int64
	path string
}

// reconciliation is the location-row change for one asset.
//
// A path that disappeared and a path that appeared for the same bytes are
// one location moving. Pairing them, in sorted path order, is what keeps a
// rename from looking like a lost file plus a new one. Leftover
// disappearances are copies that are gone. Leftover appearances are copies
// that are new.
type reconciliation struct {
	clear  []int64 // were present, were not seen; drop present before any move
	remove []int64 // missing rows sitting on a path a move is about to take
	move   []relocation
	revive []int64
	create []string
}

func reconcile(existing []locationRow, seen []string) reconciliation {
	seenSet := make(map[string]struct{}, len(seen))
	for _, p := range seen {
		seenSet[p] = struct{}{}
	}

	byPath := make(map[string]*locationRow, len(existing))
	var gone []*locationRow
	for i := range existing {
		row := &existing[i]
		byPath[row.path] = row
		if !row.present {
			continue
		}
		if _, ok := seenSet[row.path]; !ok {
			gone = append(gone, row)
		}
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i].path < gone[j].path })

	type arrival struct {
		path string
		row  *locationRow // nil when this path has no row yet
	}
	sortedSeen := append([]string(nil), seen...)
	sort.Strings(sortedSeen)
	var arrived []arrival
	for _, p := range sortedSeen {
		row := byPath[p]
		if row == nil {
			arrived = append(arrived, arrival{path: p})
			continue
		}
		if !row.present {
			arrived = append(arrived, arrival{path: p, row: row})
		}
	}

	var out reconciliation
	for _, row := range gone {
		out.clear = append(out.clear, row.id)
	}
	n := len(gone)
	if len(arrived) < n {
		n = len(arrived)
	}
	for i := 0; i < n; i++ {
		if arrived[i].row != nil {
			out.remove = append(out.remove, arrived[i].row.id)
		}
		out.move = append(out.move, relocation{id: gone[i].id, path: arrived[i].path})
	}
	for _, dest := range arrived[n:] {
		if dest.row != nil {
			out.revive = append(out.revive, dest.row.id)
		} else {
			out.create = append(out.create, dest.path)
		}
	}
	return out
}
