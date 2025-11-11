package codec

import (
	"battleship-zk/internal/game"
	"battleship-zk/internal/merkle"
	"battleship-zk/internal/zk"
)

type Secret struct {
	Board game.Board   `json:"board"`
	Tree  *merkle.Tree `json:"tree"`
	SaltHex string       `json:"salt_hex"`

}

type ShotProofPayload struct {
	Proof  []byte        `json:"proof"`
	Public zk.ShotPublic `json:"public"` // contains root and the hit and the row and col
}