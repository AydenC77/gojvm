package main

import (
	"fmt"
	"java/util"
)

type Point struct {
	X, Y int
}

func (p Point) Add(o Point) Point { return Point{p.X + o.X, p.Y + o.Y} }
func (p *Point) Move(dx, dy int)  { p.X += dx; p.Y += dy }
func (p Point) String() string    { return fmt.Sprintf("(%d,%d)", p.X, p.Y) }

type Player struct {
	Name  string
	Pos   Point
	Score float64
	Inv   *util.ArrayList
}

func NewPlayer(name string) *Player {
	return &Player{Name: name, Inv: util.NewArrayList()}
}

func (pl *Player) Pick(item string) {
	pl.Inv.Add(item)
	pl.Score += 1.5
}

func bump(p Point) Point {
	p.X = 999 // must not affect the caller
	return p
}

func main() {
	a := Point{1, 2}
	b := a // copy
	b.X = 10
	fmt.Println(a, b, a.Add(b))

	a.Move(5, 5)
	fmt.Println("moved:", a)
	c := bump(a)
	fmt.Println(a, c)

	pp := &a
	pp.Move(1, 1)
	fmt.Println(a, *pp)

	p := NewPlayer("Steve")
	p.Pick("sword")
	p.Pick("apple")
	p.Pos.Move(3, 4)
	p.Pos = a
	a.X = -1
	fmt.Println(p.Name, p.Pos, p.Score, p.Inv, p.Inv.Size())

	var zero Player
	fmt.Println(zero.Name == "", zero.Pos, zero.Inv == nil)
	var q *Player
	fmt.Println(q == nil, new(Point))
}
