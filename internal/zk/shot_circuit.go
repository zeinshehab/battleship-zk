package zk

import (
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/std/hash/mimc"
)

const MerkleDepth = 7 // 128 leaves

// ShotCircuit proves a single hit/miss matches the committed root.
type ShotCircuit struct {
	Bit  frontend.Variable              `gnark:",secret"`
	Path [MerkleDepth]frontend.Variable `gnark:",secret"`
	Dir  [MerkleDepth]frontend.Variable `gnark:",secret"`

	Root frontend.Variable `gnark:",public"`
	Hit  frontend.Variable `gnark:",public"`
}

func (c *ShotCircuit) Define(api frontend.API) error {
	api.AssertIsBoolean(c.Bit)      // Bit ∈ {0,1}
	api.AssertIsEqual(c.Hit, c.Bit) // reveal only Hit = Bit

	// leaf hash = MiMC(Bit)  (v0.14 returns (MiMC, error))
	h, err := mimc.NewMiMC(api)
	if err != nil {
		return err
	}
	h.Reset()
	h.Write(c.Bit)
	curr := h.Sum()

	// walk Merkle path
	for i := 0; i < MerkleDepth; i++ {
		h.Reset()
		isRight := c.Dir[i]

		left := api.Select(isRight, c.Path[i], curr)
		right := api.Select(isRight, curr, c.Path[i])

		h.Write(left, right)
		curr = h.Sum()
	}

	api.AssertIsEqual(curr, c.Root)
	return nil
}