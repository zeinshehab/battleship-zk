# Battleship-ZK

The defender commits to a hidden board, and for every shot returns a zero-knowledge proof of hit/miss that anyone can verify without revealing the board.

- CLI: `init`, `commit`, `shoot`, `verify`

## Install & Build
- You need Go version 1.24 minimum
- go mod tidy
- go build -o battleship ./cmd/battleship

---

## How It Works

- Commit: The defender turns a 10×10 bitboard (1=ship, 0=water) into a 128-leaf MiMC Merkle tree and shares the root and the verification key.
- Shot: The attacker calls a coordinate (r,c). The defender returns a Groth16 proof that the bit at (r,c) opens to the committed root, revealing only HIT/MISS.
- Verify: Anyone verifies the proof using the defender’s verifying key and root.

The root is the identifier of the board being attacked so if you use a different root for another board it will fail.

## Usage (One player)

- Generate a random valid board (total 17 ship cells: 5+4+3+3+2)
`./battleship init --out board.json`

- Commit to the board (build Merkle tree, create/ensure ZK keys)
`./battleship commit --board board.json --secret secret.json --keys ./keys`
You can copy the root key that it generates so you can use it to verify later.

- Produce a proof for a shot (row,col in 0..9)
`./battleship shoot --secret secret.json --keys ./keys --row 3 --col 7 --out proof_3_7.json`

- Verify the proof (public verification)
`./battleship verify --vk ./keys/shot.vk --root 0x<ROOT_FROM_COMMIT> --row 3 --col 7 --proof proof_3_7.json`


## Usage (Two player)
Same thing but each player has his own board and keys this time

### Setup (we do this once per player)

#### Player A:
```
./battleship init   --out boardA.json
./battleship commit --board boardA.json --secret secretA.json --keys ./keysA
# Share with B:  ROOT_A  and  ./keysA/shot.vk
# Keep private:  secretA.json  and  ./keysA/shot.pk
```

#### Player B:
```
./battleship init   --out boardB.json
./battleship commit --board boardB.json --secret secretB.json --keys ./keysB
# Share with A:  ROOT_B  and  ./keysB/shot.vk
# Keep private:  secretB.json  and  ./keysB/shot.pk
```

### Turns (A attacks B)

#### Defender B produces a proof:
`./battleship shoot --secret secretB.json --keys ./keysB --row r --col c --out proof_r_c.json`

#### Attacker A verifies using B's root
`./battleship verify --vk ./keysB/shot.vk --root 0xROOT_B --row r --col c --proof proof_r_c.json`

Then we just swap the roles for A to defend and B to attack.