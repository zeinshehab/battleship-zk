package merkle

import (
	"errors"
	"math/big"

	bnmimc "github.com/consensys/gnark-crypto/ecc/bn254/fr/mimc"
)

// --- encode BN254 field elements as 32-byte big-endian ---
func feBytes(x *big.Int) []byte {
	b := x.Bytes()
	if len(b) == 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func bytesToFE(b []byte) *big.Int { return new(big.Int).SetBytes(b) }

// MiMC helpers (off-chain), consistent with in-circuit MiMC
func HashLeafMiMC(bit uint8) *big.Int {
	h := bnmimc.NewMiMC()
	h.Write(feBytes(new(big.Int).SetUint64(uint64(bit))))
	return bytesToFE(h.Sum(nil))
}

func HashNodeMiMC(left, right *big.Int) *big.Int {
	h := bnmimc.NewMiMC()
	h.Write(feBytes(left))
	h.Write(feBytes(right))
	return bytesToFE(h.Sum(nil))
}

// Fixed-size binary Merkle tree stored level-by-level.
type Tree struct {
	Depth  int           `json:"depth"`
	Levels [][]*big.Int  `json:"levels"` // Levels[0]=leaves, Levels[Depth]=root
}

func BuildFixedTree(leavesBits []uint8, size int, padLeaf *big.Int,
	hashMerge func(*big.Int, *big.Int) *big.Int) (*Tree, error) {

	if size&(size-1) != 0 {
		return nil, errors.New("size must be power of two")
	}
	if len(leavesBits) > size {
		return nil, errors.New("too many leaves")
	}

	levels := make([][]*big.Int, 0)

	// Level 0: leaves
	L0 := make([]*big.Int, size)
	for i := 0; i < size; i++ {
		if i < len(leavesBits) {
			L0[i] = HashLeafMiMC(leavesBits[i])
		} else {
			L0[i] = new(big.Int).Set(padLeaf)
		}
	}
	levels = append(levels, L0)

	// Build up
	n := size
	for n > 1 {
		n2 := n / 2
		up := make([]*big.Int, n2)
		prev := levels[len(levels)-1]
		for i := 0; i < n2; i++ {
			up[i] = hashMerge(prev[2*i], prev[2*i+1])
		}
		levels = append(levels, up)
		n = n2
	}

	return &Tree{Depth: len(levels) - 1, Levels: levels}, nil
}

func (t *Tree) Root() *big.Int { return new(big.Int).Set(t.Levels[len(t.Levels)-1][0]) }

// Path returns sibling hashes + direction bits for index idx.
// dir[i]=0 ⇒ current is left child; dir[i]=1 ⇒ current is right child.
func (t *Tree) Path(idx int) (path []*big.Int, dir []uint8, err error) {
	if idx < 0 || idx >= len(t.Levels[0]) {
		return nil, nil, errors.New("idx OOB")
	}
	path = make([]*big.Int, 0, t.Depth)
	dir = make([]uint8, 0, t.Depth)
	cur := idx
	for level := 0; level < t.Depth; level++ {
		isRight := cur%2 == 1
		var sib int
		if isRight {
			sib = cur - 1
		} else {
			sib = cur + 1
		}
		path = append(path, new(big.Int).Set(t.Levels[level][sib]))
		if isRight {
			dir = append(dir, 1)
		} else {
			dir = append(dir, 0)
		}
		cur /= 2
	}
	return path, dir, nil
}