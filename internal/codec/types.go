package codec

import (
	"battleship-zk/internal/game"
	"battleship-zk/internal/merkle"
	"battleship-zk/internal/zk"
)

// Secret is the defender's private state saved by `commit` and read by `shoot`.
type Secret struct {
	Board game.Board   `json:"board"`
	Tree  *merkle.Tree `json:"tree"`
}

// ShotProofPayload is what `shoot` writes and `verify` reads.
type ShotProofPayload struct {
	Proof  []byte        `json:"proof"`  // base64 by default in JSON
	Public zk.ShotPublic `json:"public"` // contains Root (*big.Int) and Hit (0/1)
}