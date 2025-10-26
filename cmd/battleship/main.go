package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"

	"battleship-zk/internal/codec"
	"battleship-zk/internal/game"
	"battleship-zk/internal/merkle"
	"battleship-zk/internal/zk"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	switch os.Args[1] {
	case "init":
		cmdInit()
	case "commit":
		cmdCommit()
	case "shoot":
		cmdShoot()
	case "verify":
		cmdVerify()
	default:
		usage()
	}
}

func usage() {
	fmt.Println(`Battleship-ZK CLI

Commands:
  init   --out board.json
  commit --board board.json --secret secret.json --keys ./keys
  shoot  --secret secret.json --keys ./keys --row R --col C --out proof.json
  verify --vk ./keys/shot.vk --root ROOT_HEX --proof proof.json
`)
}

func cmdInit() {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	out := fs.String("out", "board.json", "output board file")
	_ = fs.Parse(os.Args[2:])

	b, err := game.GenerateRandomBoard()
	if err != nil { log.Fatal(err) }
	if err := saveJSON(*out, b); err != nil { log.Fatal(err) }
	fmt.Println("✓ wrote", *out)
}

func cmdCommit() {
	fs := flag.NewFlagSet("commit", flag.ExitOnError)
	boardPath := fs.String("board", "board.json", "board file")
	secretPath := fs.String("secret", "secret.json", "defender secret state")
	keysDir := fs.String("keys", "./keys", "keys directory")
	_ = fs.Parse(os.Args[2:])

	var b game.Board
	if err := loadJSON(*boardPath, &b); err != nil { log.Fatal(err) }
	if err := b.Validate(); err != nil { log.Fatal(err) }

	// Build Merkle tree (MiMC)
	leafHash := func(v uint8) *big.Int { return merkle.HashLeafMiMC(v) }
	zeroLeaf := leafHash(0)
	t, err := merkle.BuildFixedTree(b.Flatten(), 128, zeroLeaf, merkle.HashNodeMiMC)
	if err != nil { log.Fatal(err) }
	root := t.Root()
	fmt.Println("ROOT:", fmt.Sprintf("0x%x", root))

	// Create ZK keys (or load if exist)
	if err := zk.EnsureShotKeys(*keysDir); err != nil { log.Fatal(err) }

	// Save defender secret (board + tree)
	sec := codec.Secret{
		Board: b,
		Tree:  t,
	}
	if err := saveJSON(*secretPath, &sec); err != nil { log.Fatal(err) }
	fmt.Println("✓ wrote", *secretPath)
}

func cmdShoot() {
	fs := flag.NewFlagSet("shoot", flag.ExitOnError)
	secretPath := fs.String("secret", "secret.json", "defender secret state")
	keysDir := fs.String("keys", "./keys", "keys directory")
	row := fs.Int("row", 0, "row [0..9]")
	col := fs.Int("col", 0, "col [0..9]")
	out := fs.String("out", "proof.json", "proof output")
	_ = fs.Parse(os.Args[2:])

	var sec codec.Secret
	if err := loadJSON(*secretPath, &sec); err != nil { log.Fatal(err) }
	if *row < 0 || *row > 9 || *col < 0 || *col > 9 { log.Fatal("row/col out of range") }
	idx := *row*10 + *col

	bit := sec.Board.Cells[*row][*col]
	path, dir, err := sec.Tree.Path(idx)
	if err != nil { log.Fatal(err) }
	if len(path) != zk.MerkleDepth || len(dir) != zk.MerkleDepth { log.Fatal("bad path length") }

	proof, pub, err := zk.ProveShot(*keysDir, bit, idx, path, dir, sec.Tree.Root())
	if err != nil { log.Fatal(err) }

	payload := codec.ShotProofPayload{ Proof: proof, Public: pub }
	if err := saveJSON(*out, &payload); err != nil { log.Fatal(err) }
	fmt.Printf("✓ wrote %s (result: %s)\n", *out, map[uint8]string{0:"MISS",1:"HIT"}[bit])
}

func cmdVerify() {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	vkPath := fs.String("vk", "./keys/shot.vk", "verifying key file")
	rootHex := fs.String("root", "", "root hex prefixed 0x")
	proofPath := fs.String("proof", "proof.json", "proof payload json")
	_ = fs.Parse(os.Args[2:])

	if *rootHex == "" { log.Fatal("--root required") }
	root, ok := new(big.Int).SetString((*rootHex)[2:], 16)
	if !ok { log.Fatal("invalid root hex") }

	var payload codec.ShotProofPayload
	if err := loadJSON(*proofPath, &payload); err != nil { log.Fatal(err) }

	res, err := zk.VerifyShot(*vkPath, payload.Proof, payload.Public, root)
	if err != nil { log.Fatal(err) }
	if !res { log.Fatal(errors.New("invalid proof")) }
	if payload.Public.Hit != 0 && payload.Public.Hit != 1 { log.Fatal("invalid hit") }
	fmt.Println(map[uint8]string{0:"MISS",1:"HIT"}[payload.Public.Hit])
}

func saveJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil { return err }
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func loadJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil { return err }
	defer f.Close()
	dec := json.NewDecoder(f)
	return dec.Decode(v)
}