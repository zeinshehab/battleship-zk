package app

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"battleship-zk/internal/codec"
	"battleship-zk/internal/game"
	"battleship-zk/internal/merkle"
	"battleship-zk/internal/zk"
)

type CommitResult struct {
	RootHex string
	Secret  codec.Secret
}

func InitBoard() (game.Board, error) {
	return game.GenerateRandomBoard()
}

func Commit(b game.Board, keysDir string) (*CommitResult, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}

	// Build Merkle tree (MiMC)
	leafHash := func(v uint8) *big.Int { return merkle.HashLeafMiMC(v) }
	zeroLeaf := leafHash(0)
	t, err := merkle.BuildFixedTree(b.Flatten(), 128, zeroLeaf, merkle.HashNodeMiMC)
	if err != nil {
		return nil, err
	}
	treeRoot := t.Root()

	// Generate 32-byte salt and compute salted root = H(salt, treeRoot)
	saltBytes := make([]byte, 32)
	if _, err := rand.Read(saltBytes); err != nil {
		return nil, err
	}
	salt := new(big.Int).SetBytes(saltBytes)
	saltedRoot := merkle.HashNodeMiMC(salt, treeRoot)
	rootHex := fmt.Sprintf("0x%x", saltedRoot)

	// Ensure keys exist
	if err := zk.EnsureShotKeys(keysDir); err != nil {
		return nil, err
	}

	// Persist secret (board + tree + salt)
	sec := codec.Secret{
		Board:   b,
		Tree:    t,
		SaltHex: fmt.Sprintf("0x%x", salt),
	}

	return &CommitResult{RootHex: rootHex, Secret: sec}, nil
}

type ShootResult struct {
	Payload codec.ShotProofPayload
	Bit     uint8 // redundant with Public.Hit, kept for compatibility
}

func Shoot(sec codec.Secret, keysDir string, row, col int) (*ShootResult, error) {
	if row < 0 || row > 9 || col < 0 || col > 9 {
		return nil, fmt.Errorf("row/col out of range")
	}
	if sec.SaltHex == "" || len(sec.SaltHex) < 3 || sec.SaltHex[:2] != "0x" {
		return nil, fmt.Errorf("missing or invalid salt in secret")
	}

	// Parse salt and compute proof with (treeRoot, salt)
	salt, ok := new(big.Int).SetString(sec.SaltHex[2:], 16)
	if !ok {
		return nil, fmt.Errorf("cannot parse salt hex")
	}
	treeRoot := sec.Tree.Root()

	idx := row*10 + col
	bit := sec.Board.Cells[row][col]
	path, dir, err := sec.Tree.Path(idx)
	if err != nil {
		return nil, err
	}
	if len(path) != zk.MerkleDepth || len(dir) != zk.MerkleDepth {
		return nil, fmt.Errorf("bad path length")
	}

	// Matches client/server code path: include salt
	proof, pub, err := zk.ProveShot(keysDir, bit, idx, path, dir, treeRoot, salt)
	if err != nil {
		return nil, err
	}

	return &ShootResult{
		Payload: codec.ShotProofPayload{Proof: proof, Public: pub},
		Bit:     bit,
	}, nil
}

type VerifyResult struct {
	Valid bool
	Hit   uint8
}

// VerifyWithRoot: ensure the public Root is set to the trusted root we're given
func VerifyWithRoot(vkPath string, root *big.Int, payload codec.ShotProofPayload) (*VerifyResult, error) {
	// Some callers (e.g., HTTP verify handler) sanitize the payload and remove public.root
	// to avoid JS sci-notation issues. Populate it here from the trusted root param.
	if payload.Public.Root == nil {
		payload.Public.Root = new(big.Int).Set(root)
	} else if payload.Public.Root.Sign() == 0 {
		// In case it's a zero-value big.Int (value type), copy into it.
		payload.Public.Root = new(big.Int).Set(root)
	}

	res, err := zk.VerifyShot(vkPath, payload.Proof, payload.Public, root)
	if err != nil {
		return nil, err
	}
	if payload.Public.Hit != 0 && payload.Public.Hit != 1 {
		return nil, fmt.Errorf("invalid hit public output")
	}
	return &VerifyResult{Valid: res, Hit: payload.Public.Hit}, nil
}
