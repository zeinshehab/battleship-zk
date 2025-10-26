package zk

import (
	"bytes"
	"errors"
	"math/big"
	"os"

	"github.com/consensys/gnark/backend/groth16"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
)

type ShotPublic struct {
	Root *big.Int `json:"root"`
	Hit  uint8    `json:"hit"`
}

// Ensure proving/verifying keys exist (reads/writes via io.ReaderFrom / io.WriterTo).
func EnsureShotKeys(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	vkPath := dir + "/shot.vk"
	pkPath := dir + "/shot.pk"

	// If both key files exist AND can be parsed, reuse them; else regenerate.
	if vk, pk, err := readKeys(vkPath, pkPath); err == nil && vk != nil && pk != nil {
		return nil
	}

	// Compile circuit once
	var circuit ShotCircuit
	cs, err := frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, &circuit)
	if err != nil {
		return err
	}

	// Setup
	pk, vk, err := groth16.Setup(cs)
	if err != nil {
		return err
	}

	// Write keys
	if err := writeVK(vkPath, vk); err != nil {
		return err
	}
	if err := writePK(pkPath, pk); err != nil {
		return err
	}
	return nil
}

// Prove one shot.
func ProveShot(keysDir string, bit uint8, idx int, path []*big.Int, dir []uint8, root *big.Int) ([]byte, ShotPublic, error) {
	if len(path) != MerkleDepth || len(dir) != MerkleDepth {
		return nil, ShotPublic{}, errors.New("bad path length")
	}

	// Witness assignment for the full circuit
	var assign ShotCircuit
	assign.Bit = bit
	for i := 0; i < MerkleDepth; i++ {
		assign.Path[i] = path[i]
		assign.Dir[i] = dir[i]
	}
	assign.Root = root
	assign.Hit = bit

	// Compile and load PK
	var circuit ShotCircuit
	cs, err := frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, &circuit)
	if err != nil {
		return nil, ShotPublic{}, err
	}
	pk, err := readPK(keysDir + "/shot.pk")
	if err != nil {
		return nil, ShotPublic{}, err
	}

	// Full witness and prove
	fullWit, err := frontend.NewWitness(&assign, ecc.BN254.ScalarField())
	if err != nil {
		return nil, ShotPublic{}, err
	}
	proof, err := groth16.Prove(cs, pk, fullWit)
	if err != nil {
		return nil, ShotPublic{}, err
	}

	// Serialize proof
	var buf bytes.Buffer
	if _, err := proof.WriteTo(&buf); err != nil {
		return nil, ShotPublic{}, err
	}
	return buf.Bytes(), ShotPublic{Root: new(big.Int).Set(root), Hit: bit}, nil
}

// Verify a shot proof. (Verify returns only error; nil => valid)
func VerifyShot(vkPath string, proofBin []byte, pub ShotPublic, root *big.Int) (bool, error) {
	if pub.Root == nil {
		return false, errors.New("proof payload missing public root")
	}
	if pub.Root.Cmp(root) != 0 {
		return false, errors.New("root mismatch: proof root != --root")
	}

	// Build a PUBLIC-ONLY witness using the actual circuit type (so it implements frontend.Circuit).
	var pubAssign ShotCircuit
	pubAssign.Root = root
	pubAssign.Hit = pub.Hit

	pubWit, err := frontend.NewWitness(&pubAssign, ecc.BN254.ScalarField(), frontend.PublicOnly())
	if err != nil {
		return false, err
	}

	// Read VK and proof
	vk, err := readVK(vkPath)
	if err != nil {
		return false, err
	}
	pr := groth16.NewProof(ecc.BN254)
	if _, err := pr.ReadFrom(bytes.NewReader(proofBin)); err != nil {
		return false, err
	}

	if err := groth16.Verify(pr, vk, pubWit); err != nil {
		return false, err
	}
	return true, nil
}

// --- key IO helpers using io.WriterTo / io.ReaderFrom ---

func writeVK(path string, vk groth16.VerifyingKey) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = vk.WriteTo(f)
	return err
}

func writePK(path string, pk groth16.ProvingKey) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = pk.WriteTo(f)
	return err
}

func readVK(path string) (groth16.VerifyingKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vk := groth16.NewVerifyingKey(ecc.BN254)
	_, err = vk.ReadFrom(f)
	return vk, err
}

func readPK(path string) (groth16.ProvingKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pk := groth16.NewProvingKey(ecc.BN254)
	_, err = pk.ReadFrom(f)
	return pk, err
}

func readKeys(vkPath, pkPath string) (groth16.VerifyingKey, groth16.ProvingKey, error) {
	vk, err := readVK(vkPath)
	if err != nil {
		return nil, nil, err
	}
	pk, err := readPK(pkPath)
	if err != nil {
		return nil, nil, err
	}
	return vk, pk, nil
}