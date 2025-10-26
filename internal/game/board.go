package game

import (
	"errors"
	"math/rand"
)

// Board is a 10x10 grid. Cell: 0=water, 1=ship.
type Board struct { Cells [10][10]uint8 }

var shipSizes = []int{5,4,3,3,2} // total 17

func (b *Board) Validate() error {
	// zero/one and count==17
	total := 0
	for r:=0; r<10; r++ { for c:=0; c<10; c++ {
		v := b.Cells[r][c]
		if v != 0 && v != 1 { return errors.New("board has non-binary cell") }
		total += int(v)
	}}
	if total != 17 { return errors.New("board must contain exactly 17 ship cells") }
	return nil
}

func (b *Board) Flatten() []uint8 {
	out := make([]uint8, 100)
	k:=0
	for r:=0; r<10; r++ { for c:=0; c<10; c++ {
		out[k] = b.Cells[r][c]
		k++
	}}
	return out
}

// GenerateRandomBoard places standard ships without overlap (no adjacency rule enforced for MVP).
func GenerateRandomBoard() (Board, error) {
	var b Board
	tries := 0
	for _, L := range shipSizes {
	retry:
		if tries > 10000 { return Board{}, errors.New("failed to place ships") }
		tries++
		vert := rand.Intn(2) == 0
		r := rand.Intn(10)
		c := rand.Intn(10)
		if vert {
			if r+L > 10 { goto retry }
			for i:=0; i<L; i++ { if b.Cells[r+i][c] == 1 { goto retry } }
			for i:=0; i<L; i++ { b.Cells[r+i][c] = 1 }
		} else {
			if c+L > 10 { goto retry }
			for i:=0; i<L; i++ { if b.Cells[r][c+i] == 1 { goto retry } }
			for i:=0; i<L; i++ { b.Cells[r][c+i] = 1 }
		}
	}
	return b, nil
}