package levels

import "fmt"

// Each brush placement below is a deliberate part of an original layout. These
// are compact authoring instructions, not a random or seed-dependent generator.
// Rectangle endpoints are inclusive and use zero-based tile coordinates.
type layout [Size][Size]byte

func newLayout() *layout {
	grid := &layout{}
	grid.rect(0, 0, Size-1, Size-1, '.')
	grid.rect(0, 0, Size-1, 0, '#')
	grid.rect(0, Size-1, Size-1, Size-1, '#')
	grid.rect(0, 0, 0, Size-1, '#')
	grid.rect(Size-1, 0, Size-1, Size-1, '#')
	return grid
}

func (grid *layout) rect(x1, y1, x2, y2 int, tile byte) {
	if x1 < 0 || y1 < 0 || x2 >= Size || y2 >= Size || x1 > x2 || y1 > y2 {
		panic("invalid authored level rectangle")
	}
	for y := y1; y <= y2; y++ {
		for x := x1; x <= x2; x++ {
			grid[y][x] = tile
		}
	}
}

func (grid *layout) ring(x1, y1, x2, y2 int, tile byte) {
	grid.rect(x1, y1, x2, y1, tile)
	grid.rect(x1, y2, x2, y2, tile)
	grid.rect(x1, y1, x1, y2, tile)
	grid.rect(x2, y1, x2, y2, tile)
}

func (grid *layout) rows() []string {
	rows := make([]string, Size)
	for y := range grid {
		rows[y] = string(grid[y][:])
	}
	return rows
}

func arena(id int, name, subtitle, description string, difficulty int, theme string, count, active int, base, player Point, draw func(*layout)) Level {
	grid := newLayout()
	draw(grid)
	spawns := []Point{{2.5, 2.5}, {11.5, 2.5}, {21.5, 2.5}}
	for _, point := range append([]Point{player}, spawns...) {
		grid.rect(int(point.X)-1, int(point.Y)-1, int(point.X)+1, int(point.Y)+1, '.')
	}
	grid.rect(int(base.X), int(base.Y), int(base.X), int(base.Y), '.')
	return Level{
		ID: id, Name: name, Subtitle: subtitle, Description: description,
		Difficulty: difficulty, Theme: theme, EnemyCount: count, MaxActive: active,
		Base: base, PlayerSpawn: player, EnemySpawns: spawns, Grid: grid.rows(),
	}
}

func makeCatalog() []Level {
	levels := []Level{
		arena(1, "First Light", "The proving ground", "A quiet depot with wide escape routes. Learn to break brick cover, meet the first wave, and protect the relay.", 1, "#E8B567", 6, 2, Point{12.5, 21.5}, Point{8.5, 21.5}, func(g *layout) {
			g.rect(5, 6, 8, 7, '+')
			g.rect(15, 6, 18, 7, '+')
			g.rect(10, 10, 13, 11, '#')
			g.rect(3, 12, 5, 13, '+')
			g.rect(18, 12, 20, 13, '+')
			g.rect(7, 16, 9, 17, '+')
			g.rect(14, 16, 16, 17, '+')
			g.rect(3, 16, 4, 18, 'g')
			g.rect(19, 16, 20, 18, 'g')
		}),
		arena(2, "Switchyard", "Choose your line", "Staggered steel tracks divide the yard. Change lanes through the gaps or cut through brick sidings to intercept a flank.", 1, "#DF8B62", 8, 2, Point{7.5, 21.5}, Point{11.5, 21.5}, func(g *layout) {
			g.rect(5, 5, 5, 10, '#')
			g.rect(5, 14, 5, 18, '#')
			g.rect(11, 7, 11, 15, '#')
			g.rect(17, 5, 17, 9, '#')
			g.rect(17, 13, 17, 18, '#')
			g.rect(7, 7, 9, 8, '+')
			g.rect(13, 11, 15, 12, '+')
			g.rect(7, 16, 9, 17, '+')
			g.rect(19, 7, 21, 8, '+')
			g.rect(2, 14, 3, 17, 'g')
			g.rect(19, 15, 21, 17, 'g')
		}),
		arena(3, "Canal Crossing", "Hold the bridges", "Two canals split the field into three banks. Bullets cross water; tanks need a bridge. Keep an escape route behind your firing position.", 2, "#70B6C3", 8, 3, Point{16.5, 21.5}, Point{12.5, 21.5}, func(g *layout) {
			g.rect(1, 8, 22, 9, '~')
			g.rect(4, 8, 6, 9, '.')
			g.rect(16, 8, 18, 9, '.')
			g.rect(1, 14, 22, 15, '~')
			g.rect(9, 14, 12, 15, '.')
			g.rect(20, 14, 22, 15, '.')
			g.rect(9, 5, 13, 6, '+')
			g.rect(3, 11, 5, 12, '#')
			g.rect(15, 11, 17, 12, '+')
			g.rect(5, 17, 7, 18, '+')
			g.rect(14, 17, 16, 18, '#')
		}),
		arena(4, "Glasshouse", "Read the canopy", "Overgrown beds conceal movement around a broken greenhouse. Clear a sightline through brick and watch for a second approach.", 2, "#95B97A", 10, 3, Point{11.5, 21.5}, Point{7.5, 21.5}, func(g *layout) {
			g.rect(3, 5, 7, 9, 'g')
			g.rect(15, 5, 20, 9, 'g')
			g.rect(3, 13, 7, 18, 'g')
			g.rect(15, 13, 20, 18, 'g')
			g.ring(9, 7, 13, 16, '+')
			g.rect(10, 7, 12, 7, '.')
			g.rect(9, 11, 9, 12, '.')
			g.rect(13, 13, 13, 14, '.')
			g.rect(10, 16, 12, 16, '.')
			g.rect(10, 10, 11, 10, '#')
			g.rect(4, 7, 5, 8, '#')
			g.rect(17, 7, 18, 8, '#')
			g.rect(4, 15, 5, 16, '+')
			g.rect(17, 15, 18, 16, '+')
		}),
		arena(5, "The Kilns", "Make your own doorway", "Four brick workshops turn every volley into a new route. Steel furnace cores remain after the walls fall, so keep moving around them.", 3, "#C9866E", 10, 3, Point{17.5, 21.5}, Point{13.5, 21.5}, func(g *layout) {
			for _, origin := range [][2]int{{3, 5}, {14, 5}, {3, 13}, {14, 13}} {
				x, y := origin[0], origin[1]
				g.ring(x, y, x+6, y+5, '+')
				g.rect(x+2, y, x+3, y, '.')
				g.rect(x+3, y+5, x+4, y+5, '.')
				g.rect(x+2, y+2, x+3, y+3, '#')
			}
			g.rect(11, 8, 12, 9, 'g')
			g.rect(11, 14, 12, 16, 'g')
		}),
		arena(6, "Split Current", "Protect the far bank", "A long spillway separates two unequal lanes. Cross at the cutouts, then return to a relay tucked away on the western bank.", 3, "#6BAAAB", 12, 3, Point{6.5, 21.5}, Point{10.5, 21.5}, func(g *layout) {
			g.rect(10, 5, 12, 18, '~')
			g.rect(10, 8, 12, 10, '.')
			g.rect(10, 15, 12, 16, '.')
			g.rect(4, 6, 6, 8, '+')
			g.rect(3, 12, 6, 13, '#')
			g.rect(6, 16, 8, 17, '+')
			g.rect(16, 6, 19, 7, '#')
			g.rect(15, 11, 18, 12, '+')
			g.rect(18, 16, 20, 18, '+')
			g.rect(14, 14, 16, 17, 'g')
		}),
		arena(7, "Crossfire", "Mind the diagonals", "Offset steel islands create overlapping firing lanes. Use their corners for cover and avoid committing to a single long corridor.", 4, "#C6A16D", 12, 4, Point{12.5, 21.5}, Point{8.5, 21.5}, func(g *layout) {
			for _, p := range [][2]int{{4, 5}, {17, 5}, {7, 8}, {14, 8}, {10, 11}, {7, 14}, {14, 14}, {4, 17}, {17, 17}} {
				g.rect(p[0], p[1], p[0]+1, p[1]+1, '#')
			}
			g.rect(10, 5, 12, 6, '+')
			g.rect(3, 10, 5, 11, '+')
			g.rect(18, 10, 20, 11, '+')
			g.rect(10, 17, 12, 18, '+')
			g.rect(2, 14, 4, 15, 'g')
			g.rect(19, 14, 21, 15, 'g')
		}),
		arena(8, "Relay Station", "Circle the strongpoint", "An abandoned central station splits the assault into side routes. The eastern relay needs protection while brick screens gradually open the perimeter.", 4, "#9A9DC5", 14, 4, Point{18.5, 21.5}, Point{14.5, 21.5}, func(g *layout) {
			g.rect(9, 6, 14, 8, '#')
			g.rect(9, 14, 14, 16, '#')
			g.rect(9, 9, 10, 13, '#')
			g.rect(13, 9, 14, 13, '#')
			g.rect(11, 6, 12, 8, '.')
			g.rect(11, 14, 12, 16, '.')
			g.rect(3, 6, 5, 7, '+')
			g.rect(5, 11, 7, 12, '+')
			g.rect(3, 16, 5, 17, '+')
			g.rect(18, 6, 20, 7, '+')
			g.rect(16, 11, 18, 12, '+')
			g.rect(18, 16, 20, 17, '+')
			g.rect(2, 9, 3, 13, 'g')
			g.rect(20, 9, 21, 13, 'g')
		}),
		arena(9, "Fault Line", "Find the crossing", "A fractured watercourse cuts across the arena. Two broad breaches offer passage; the lower bank rewards quick changes of direction.", 5, "#D19C82", 14, 4, Point{7.5, 21.5}, Point{11.5, 21.5}, func(g *layout) {
			g.rect(1, 5, 5, 6, '~')
			g.rect(5, 6, 8, 8, '~')
			g.rect(8, 8, 11, 10, '~')
			g.rect(11, 10, 14, 12, '~')
			g.rect(14, 12, 17, 14, '~')
			g.rect(17, 14, 20, 16, '~')
			g.rect(20, 16, 22, 18, '~')
			g.rect(6, 6, 8, 9, '.')
			g.rect(15, 12, 17, 15, '.')
			g.rect(13, 5, 15, 6, '+')
			g.rect(19, 8, 20, 10, '#')
			g.rect(3, 10, 5, 11, '+')
			g.rect(5, 15, 8, 16, '#')
			g.rect(11, 17, 14, 18, '+')
			g.rect(2, 15, 3, 18, 'g')
			g.rect(16, 5, 18, 7, 'g')
		}),
		arena(10, "Last Broadcast", "Keep the signal alive", "A final assault through a broken communications compound. Inner gates, outer flanks, and changing brick cover demand constant attention to the relay.", 5, "#E0AD58", 16, 4, Point{12.5, 21.5}, Point{8.5, 21.5}, func(g *layout) {
			g.ring(4, 5, 19, 18, '#')
			g.rect(7, 5, 9, 5, '.')
			g.rect(14, 5, 16, 5, '.')
			g.rect(4, 9, 4, 11, '.')
			g.rect(19, 12, 19, 14, '.')
			g.rect(7, 18, 9, 18, '.')
			g.rect(14, 18, 16, 18, '.')
			g.ring(8, 9, 15, 14, '+')
			g.rect(11, 9, 12, 9, '.')
			g.rect(8, 11, 8, 12, '.')
			g.rect(15, 11, 15, 12, '.')
			g.rect(11, 14, 12, 14, '.')
			g.rect(11, 11, 12, 12, '#')
			g.rect(2, 9, 3, 12, 'g')
			g.rect(20, 12, 21, 15, 'g')
			g.rect(6, 7, 7, 8, '+')
			g.rect(16, 15, 17, 16, '+')
		}),
	}
	for _, level := range levels {
		if err := Validate(level); err != nil {
			panic(fmt.Sprintf("invalid built-in arena: %v", err))
		}
	}
	return levels
}
