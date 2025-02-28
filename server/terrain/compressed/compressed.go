// SPDX-FileCopyrightText: 2021 Softbear, Inc.
// SPDX-License-Identifier: AGPL-3.0-or-later

package compressed

import (
	"fmt"
	"math/rand"
	"mk48/server/terrain"
	"mk48/server/world"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	Size       = 2048
	chunkCount = Size / chunkSize
)

// Coordinate types
// World coordinates:            world.Entity.Position
// Terrain coordinates:          World coordinates / terrain.Scale
// Unsigned terrain coordinates: Terrain coordinates + Size / 2
// Chunk coordinates:            Unsigned terrain coordinates / chunkSize

// Terrain is a compressed implementation of terrain.Terrain.
// It is safe to call all methods on it concurrently.
type Terrain struct {
	generator  terrain.Source
	chunks     [chunkCount][chunkCount]*chunk
	chunkCount int32
	mutex      sync.Mutex
}

func New(generator terrain.Source) *Terrain {
	return &Terrain{
		generator: generator,
	}
}

// clamp returns a signed terrain coords aabb and unsigned terrain coords ux, uy, width, and height.
func (t *Terrain) clamp(aabb world.AABB) (aabb2 world.AABB, ux, uy, width, height uint) {
	p := aabb.Vec2f.Mul(1.0 / terrain.Scale).Floor()

	// Signed terrain coords
	x := maxInt(-Size/2, int(p.X))
	y := maxInt(-Size/2, int(p.Y))

	if x >= Size/2 || y >= Size/2 {
		return
	}

	s := world.Vec2f{X: aabb.Width, Y: aabb.Height}.Mul(1.0 / terrain.Scale).Ceil()
	ux = uint(x + Size/2)
	uy = uint(y + Size/2)
	width = uint(minInt(Size/2-x, int(s.X)+1))
	height = uint(minInt(Size/2-y, int(s.Y)+1))

	aabb2 = world.AABB{
		Vec2f: world.Vec2f{
			X: float32(x),
			Y: float32(y),
		}.Mul(terrain.Scale),
		Width:  float32(width-1) * terrain.Scale,
		Height: float32(height-1) * terrain.Scale,
	}

	return
}

func (t *Terrain) Clamp(aabb world.AABB) world.AABB {
	clamped, _, _, _, _ := t.clamp(aabb)
	return clamped
}

func (t *Terrain) At(aabb world.AABB) *terrain.Data {
	clamped, x, y, width, height := t.clamp(aabb)

	data := terrain.NewData()
	buffer := Buffer{
		buf: data.Data,
	}

	for j := y; j < height+y; j++ {
		for i := x; i < width+x; i++ {
			buffer.writeByte(t.at(i, j))
		}
	}

	data.AABB = clamped
	data.Data = buffer.Buffer()
	data.Stride = int(width)
	data.Length = int(width * height)

	return data
}

func (t *Terrain) Decode(data *terrain.Data) (raw []byte, err error) {
	var buf Buffer
	buf.Reset(data.Data)
	raw = make([]byte, data.Length)
	_, err = buf.Read(raw)
	return
}

func (t *Terrain) Repair() {
	millis := time.Now().UnixNano() / int64(time.Millisecond/time.Nanosecond)
	for ucx, chunks := range t.chunks {
		for ucy, c := range chunks {
			if c != nil && millis >= c.regen {
				if c.regen != 0 { // Don't regen the first time
					generateChunk(t.generator, uint(ucx)-Size/chunkSize/2, uint(ucy)-Size/chunkSize/2, c)
				}
				c.regen = millis + regenMillis + int64(rand.Intn(10000)) // add some randomness to avoid simultaneous regen
			}
		}
	}
}

func (t *Terrain) Collides(entity *world.Entity, seconds float32) bool {
	data := entity.Data()
	threshold := byte(terrain.OceanLevel)
	switch data.SubKind {
	case world.EntitySubKindMissile, world.EntitySubKindRocket, world.EntitySubKindShell:
		threshold = terrain.SandLevel
	}

	// Kludge offset
	threshold -= 6

	// Not worth doing multiple terrain samples for small entities
	if data.Radius <= terrain.Scale/5 {
		return t.AtPos(entity.Position) > threshold
	}

	sweep := seconds * entity.Velocity
	normal := entity.Direction.Vec2f()
	tangent := normal.Rot90()

	const graceMargin = 0.9
	dimensions := world.Vec2f{X: data.Length + sweep, Y: data.Width}.Mul(0.5 * graceMargin)
	position := entity.Position.AddScaled(normal, sweep*0.5)

	dx := min(terrain.Scale*2/3, dimensions.X*0.499)
	dy := min(terrain.Scale*2/3, dimensions.Y*0.499)

	for l := -dimensions.X; l <= dimensions.X; l += dx {
		for w := -dimensions.Y; w <= dimensions.Y; w += dy {
			if t.AtPos(position.AddScaled(normal, l).AddScaled(tangent, w)) > threshold {
				return true
			}
		}
	}

	return false
}

func (t *Terrain) Debug() {
	fmt.Println("compressed terrain: chunks:", t.chunkCount)
}

func (t *Terrain) AtPos(pos world.Vec2f) byte {
	pos = pos.Mul(1.0 / terrain.Scale)

	// Floor and Ceiling pos
	cPos := pos.Ceil()
	cx := uint(int(cPos.X) + Size/2)
	cy := uint(int(cPos.Y) + Size/2)
	if cx >= Size || cy >= Size {
		return 0
	}

	fPos := pos.Floor()
	fx := uint(int(fPos.X) + Size/2)
	fy := uint(int(fPos.Y) + Size/2)
	if fx >= Size || fy >= Size {
		return 0
	}

	// Sample 4x4 grid
	// 00 10
	// 01 11
	var c00, c10, c01, c11 byte

	// Fast version if in the same chunk
	if fx/chunkSize == cx/chunkSize && fy/chunkSize == cy/chunkSize {
		c := t.getChunk(fx, fy)
		c00 = c.at(fx, fy)
		c10 = c.at(cx, fy)
		c01 = c.at(fx, cy)
		c11 = c.at(cx, cy)
	} else {
		c00 = t.at(fx, fy)
		c10 = t.at(cx, fy)
		c01 = t.at(fx, cy)
		c11 = t.at(cx, cy)
	}

	delta := pos.Sub(fPos)
	return blerp(c00, c10, c01, c11, delta.X, delta.Y)
}

// Changes the terrain height at pos by change
func (t *Terrain) Sculpt(pos world.Vec2f, change float32) {
	pos = pos.Mul(1.0 / terrain.Scale)

	// Floor and Ceiling pos
	cPos := pos.Ceil()
	cx := uint(int(cPos.X) + Size/2)
	cy := uint(int(cPos.Y) + Size/2)
	if cx >= Size || cy >= Size {
		return
	}

	fPos := pos.Floor()
	fx := uint(int(fPos.X) + Size/2)
	fy := uint(int(fPos.Y) + Size/2)
	if fx >= Size || fy >= Size {
		return
	}

	delta := pos.Sub(fPos)
	change *= 0.5

	// Set 4x4 grid
	// 00 10
	// 01 11
	t.set(fx, fy, clampToGrassByte(float32(t.at(fx, fy))+change*(2-delta.X-delta.Y)))
	t.set(cx, fy, clampToGrassByte(float32(t.at(cx, fy))*change*(1+delta.X-delta.Y)))
	t.set(fx, cy, clampToGrassByte(float32(t.at(fx, cy))*change*(1-delta.X+delta.Y)))
	t.set(cx, cy, clampToGrassByte(float32(t.at(cx, cy))+change*(delta.X+delta.Y)))
}

// at gets the height of the terrain given x and y unsigned terrain coords.
func (t *Terrain) at(x, y uint) byte {
	return t.getChunk(x, y).at(x, y)
}

// at sets the height of the terrain given x, y unsigned terrain coords and the value to set it to.
func (t *Terrain) set(x, y uint, value byte) {
	t.getChunk(x, y).set(x, y, value)
}

// getChunk gets a chunk given its unsigned terrain coordinates.
// TODO figure out how to get this inlined
func (t *Terrain) getChunk(x, y uint) *chunk {
	// Elide bounds checks
	x = (x / chunkSize) & (chunkCount - 1)
	y = (y / chunkSize) & (chunkCount - 1)

	// Use atomics/mutex to make sure chunk is generated
	// Basically sync.Once for each chunk but with shared mutex
	c := (*chunk)(atomic.LoadPointer((*unsafe.Pointer)(unsafe.Pointer(&t.chunks[x][y]))))
	if c == nil {
		return t.getChunkSlow(x, y)
	}
	return c
}

func (t *Terrain) getChunkSlow(x, y uint) *chunk {
	chunkPtr := (*unsafe.Pointer)(unsafe.Pointer(&t.chunks[x][y]))

	t.mutex.Lock()
	defer t.mutex.Unlock()

	// Load again to make sure its still nil after acquiring the lock
	c := (*chunk)(atomic.LoadPointer(chunkPtr))
	if c == nil {
		// Generate chunk
		c = generateChunk(t.generator, x-chunkCount/2, y-chunkCount/2, nil)
		t.chunkCount++

		// Store pointer
		atomic.StorePointer(chunkPtr, unsafe.Pointer(c))
	}

	return c
}
