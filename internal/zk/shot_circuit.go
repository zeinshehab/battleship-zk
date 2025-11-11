package zk

import (
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/std/hash/mimc"
	"github.com/consensys/gnark/std/math/bits"
)

const MerkleDepth = 7 // 128 leaves

type ShotCircuit struct {
	Bit  frontend.Variable              `gnark:",secret"`
	Path [MerkleDepth]frontend.Variable `gnark:",secret"`
	Dir  [MerkleDepth]frontend.Variable `gnark:",secret"`
	Salt frontend.Variable `gnark:",secret"`

	Root frontend.Variable `gnark:",public"`
	Hit  frontend.Variable `gnark:",public"`
	Row  frontend.Variable `gnark:",public"`
	Col  frontend.Variable `gnark:",public"`
}

func (c *ShotCircuit) Define(api frontend.API) error {
	api.AssertIsBoolean(c.Bit)
	api.AssertIsBoolean(c.Hit)
	api.AssertIsEqual(c.Hit, c.Bit)

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

	treeRoot := curr

	hSalt, err := mimc.NewMiMC(api)
	if err != nil {
		return err
	}
	hSalt.Reset()
	hSalt.Write(c.Salt, treeRoot)
	salted := hSalt.Sum()

	api.AssertIsEqual(salted, c.Root)

	// make sure its the correct index
	idx := api.Add(api.Mul(c.Row, 10), c.Col) // idx = row*10 + col
	idxBits := bits.ToBinary(api, idx, bits.WithNbDigits(MerkleDepth))

	for i := 0; i < MerkleDepth; i++ {
		api.AssertIsBoolean(idxBits[i])
		api.AssertIsEqual(c.Dir[i], idxBits[i])
	}
	
	return nil
}