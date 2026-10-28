package game

// Navigation is a shared feature extractor, not a privileged policy. Weighted
// Dijkstra fields let a tiny actor reason about destinations without memorizing
// maps. Bricks have higher cost, steel/water are impassable. All enemies share
// these two room-level fields; recompute only on player-cell or terrain changes.

type pathNode struct {
	cell int
	cost float32
}
type pathHeap struct {
	nodes [BoardSize * BoardSize * 5]pathNode
	count int
}

func (h *pathHeap) push(n pathNode) {
	if h.count >= len(h.nodes) {
		return
	}
	i := h.count
	h.count++
	for i > 0 {
		p := (i - 1) / 2
		if h.nodes[p].cost <= n.cost {
			break
		}
		h.nodes[i] = h.nodes[p]
		i = p
	}
	h.nodes[i] = n
}

func (h *pathHeap) pop() pathNode {
	result := h.nodes[0]
	h.count--
	last := h.nodes[h.count]
	if h.count > 0 {
		i := 0
		for {
			child := i*2 + 1
			if child >= h.count {
				break
			}
			if child+1 < h.count && h.nodes[child+1].cost < h.nodes[child].cost {
				child++
			}
			if h.nodes[child].cost >= last.cost {
				break
			}
			h.nodes[i] = h.nodes[child]
			i = child
		}
		h.nodes[i] = last
	}
	return result
}

func (g *Game) refreshNavigation() {
	cell := int(g.player.Y)*BoardSize + int(g.player.X)
	if g.navDirty || cell != g.navPlayerCell {
		g.distanceField(int(g.player.X), int(g.player.Y), &g.navPlayer)
		g.navPlayerCell = cell
	}
	if g.navDirty {
		g.distanceField(int(g.base.X), int(g.base.Y), &g.navBase)
		g.navDirty = false
	}
}

func (g *Game) distanceField(tx, ty int, field *[BoardSize][BoardSize]float32) {
	for y := range field {
		for x := range field[y] {
			field[y][x] = 10000
		}
	}
	if !inside(tx, ty) {
		return
	}
	field[ty][tx] = 0
	var heap pathHeap
	heap.push(pathNode{cell: ty*BoardSize + tx, cost: 0})
	for heap.count > 0 {
		n := heap.pop()
		x, y := n.cell%BoardSize, n.cell/BoardSize
		if n.cost != field[y][x] {
			continue
		}
		for dir := 0; dir < 4; dir++ {
			nx, ny := x+int(directionX[dir]), y+int(directionY[dir])
			if !inside(nx, ny) {
				continue
			}
			tile := g.grid[ny][nx]
			if tile == '#' || tile == '~' {
				continue
			}
			cost := float32(1)
			if tile == '+' {
				cost = 5
			}
			if d := n.cost + cost; d < field[ny][nx] {
				field[ny][nx] = d
				heap.push(pathNode{cell: ny*BoardSize + nx, cost: d})
			}
		}
	}
}
