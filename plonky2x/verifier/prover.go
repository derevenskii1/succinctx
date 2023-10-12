package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/consensys/gnark-crypto/ecc"
	curve "github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/utils"
	"github.com/consensys/gnark/backend/groth16"
	groth16_bn254 "github.com/consensys/gnark/backend/groth16/bn254"
	"github.com/consensys/gnark/backend/witness"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/logger"
	"github.com/ethereum/go-ethereum/accounts/abi"
	gnark_verifier_types "github.com/succinctlabs/gnark-plonky2-verifier/types"
	"github.com/succinctlabs/gnark-plonky2-verifier/variables"

	"github.com/succinctlabs/sdk/gnarkx/types"
)

func LoadProverData(path string) (constraint.ConstraintSystem, groth16.ProvingKey, error) {
	log := logger.Logger()
	r1csFile, err := os.Open(path + "/r1cs.bin")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open r1cs file: %w", err)
	}
	r1cs := groth16.NewCS(ecc.BN254)
	start := time.Now()
	r1csReader := bufio.NewReader(r1csFile)
	_, err = r1cs.ReadFrom(r1csReader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read r1cs file: %w", err)
	}
	r1csFile.Close()
	elapsed := time.Since(start)
	log.Debug().Msg("Successfully loaded constraint system, time: " + elapsed.String())

	pkFile, err := os.Open(path + "/pk.bin")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open pk file: %w", err)
	}
	pk := groth16.NewProvingKey(ecc.BN254)
	start = time.Now()
	pkReader := bufio.NewReader(pkFile)
	_, err = pk.ReadFrom(pkReader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read pk file: %w", err)
	}
	pkFile.Close()
	elapsed = time.Since(start)
	log.Debug().Msg("Successfully loaded proving key, time: " + elapsed.String())

	return r1cs, pk, nil
}

func GetInputHashOutputHash(proofWithPis gnark_verifier_types.ProofWithPublicInputsRaw) (*big.Int, *big.Int) {
	publicInputs := proofWithPis.PublicInputs
	if len(publicInputs) != 64 {
		panic("publicInputs must be 64 bytes")
	}
	publicInputsBytes := make([]byte, 64)
	for i, v := range publicInputs {
		publicInputsBytes[i] = byte(v & 0xFF)
	}
	inputHash := new(big.Int).SetBytes(publicInputsBytes[0:32])
	outputHash := new(big.Int).SetBytes(publicInputsBytes[32:64])
	if inputHash.BitLen() > 253 {
		panic("inputHash must be at most 253 bits")
	}
	if outputHash.BitLen() > 253 {
		panic("outputHash must be at most 253 bits")
	}
	return inputHash, outputHash
}

func Prove(circuitPath string, r1cs constraint.ConstraintSystem, pk groth16.ProvingKey, vk groth16.VerifyingKey) (groth16.Proof, witness.Witness, error) {
	log := logger.Logger()

	verifierOnlyCircuitData := variables.DeserializeVerifierOnlyCircuitData(
		gnark_verifier_types.ReadVerifierOnlyCircuitData(circuitPath + "/verifier_only_circuit_data.json"),
	)
	proofWithPis := gnark_verifier_types.ReadProofWithPublicInputs(circuitPath + "/proof_with_public_inputs.json")
	proofWithPisVariable := variables.DeserializeProofWithPublicInputs(proofWithPis)

	inputHash, outputHash := GetInputHashOutputHash(proofWithPis)

	// Circuit assignment
	assignment := &Plonky2xVerifierCircuit{
		ProofWithPis:   proofWithPisVariable,
		VerifierData:   verifierOnlyCircuitData,
		VerifierDigest: verifierOnlyCircuitData.CircuitDigest,
		InputHash:      frontend.Variable(inputHash),
		OutputHash:     frontend.Variable(outputHash),
	}

	log.Debug().Msg("Generating witness")
	start := time.Now()
	witness, err := frontend.NewWitness(assignment, ecc.BN254.ScalarField())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate witness: %w", err)
	}
	elapsed := time.Since(start)
	log.Debug().Msg("Successfully generated witness, time: " + elapsed.String())

	log.Debug().Msg("Creating proof")
	start = time.Now()
	proof, err := groth16.Prove(r1cs, pk, witness)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create proof: %w", err)
	}
	elapsed = time.Since(start)
	log.Info().Msg("Successfully created proof, time: " + elapsed.String())

	const fpSize = 4 * 8
	var buf bytes.Buffer
	proof.WriteRawTo(&buf)
	proofBytes := buf.Bytes()

	// taken from test/assert_solidity.go
	proofBytes = proofBytes[:32*8]
	proofStr := hex.EncodeToString(proofBytes)
	fmt.Printf("ProofBytes: %v\n", proofStr)

	for i := 0; i < 8; i++ {
		fmt.Printf("ProofBytes[%v]: %v\n", i, proofStr[fpSize*i:fpSize*(i+1)])
	}

	pWitness, err := witness.Public()
	bPublicWitness, err := pWitness.MarshalBinary()
	bPublicWitness = bPublicWitness[12:]
	publicWitnessStr := hex.EncodeToString(bPublicWitness)
	fmt.Printf("PublicWitness: %v\n", publicWitnessStr)
	witnessVec := pWitness.Vector().(fr.Vector)
	// end of debug

	for i := 0; i < 3; i++ {
		fmt.Printf("PublicWitness[%v]: %v\n", i, hex.EncodeToString(bPublicWitness[fpSize*i:fpSize*(i+1)]))
	}

	// EXTRA LOGIC TO GET EXTRA INPUT
	vkStruct := vk.(*groth16_bn254.VerifyingKey)
	proofStruct := proof.(*groth16_bn254.Proof)

	maxNbPublicCommitted := 0
	for _, s := range vkStruct.PublicAndCommitmentCommitted { // iterate over commitments
		maxNbPublicCommitted = utils.Max(maxNbPublicCommitted, len(s))
	}
	commitmentsSerialized := make([]byte, len(vkStruct.PublicAndCommitmentCommitted)*fr.Bytes)
	commitmentPrehashSerialized := make([]byte, curve.SizeOfG1AffineUncompressed+maxNbPublicCommitted*fr.Bytes)
	for i := range vkStruct.PublicAndCommitmentCommitted { // solveCommitmentWire
		copy(commitmentPrehashSerialized, proofStruct.Commitments[i].Marshal())
		offset := curve.SizeOfG1AffineUncompressed
		for j := range vkStruct.PublicAndCommitmentCommitted[i] {
			copy(commitmentPrehashSerialized[offset:], witnessVec[vkStruct.PublicAndCommitmentCommitted[i][j]-1].Marshal())
			offset += fr.Bytes
		}
		if res, err := fr.Hash(commitmentPrehashSerialized[:offset], []byte(constraint.CommitmentDst), 1); err != nil {
			panic(err)
		} else {
			fmt.Printf("Commitment: %v\n", hex.EncodeToString(res[0].Marshal()))
			// fmt.Printf("Commitment %v: %v %v\n", i, res[0], res[0].Marshal())
			witnessVec = append(witnessVec, res[0])
			copy(commitmentsSerialized[i*fr.Bytes:], res[0].Marshal())
		}
	}
	// asdf

	output := &types.Groth16Proof{}
	output.A[0] = new(big.Int).SetBytes(proofBytes[fpSize*0 : fpSize*1])
	output.A[1] = new(big.Int).SetBytes(proofBytes[fpSize*1 : fpSize*2])
	output.B[0][0] = new(big.Int).SetBytes(proofBytes[fpSize*2 : fpSize*3])
	output.B[0][1] = new(big.Int).SetBytes(proofBytes[fpSize*3 : fpSize*4])
	output.B[1][0] = new(big.Int).SetBytes(proofBytes[fpSize*4 : fpSize*5])
	output.B[1][1] = new(big.Int).SetBytes(proofBytes[fpSize*5 : fpSize*6])
	output.C[0] = new(big.Int).SetBytes(proofBytes[fpSize*6 : fpSize*7])
	output.C[1] = new(big.Int).SetBytes(proofBytes[fpSize*7 : fpSize*8])

	// abi.encode(proof.A, proof.B, proof.C)
	uint256Array, err := abi.NewType("uint256[2]", "", nil)
	if err != nil {
		log.Fatal().AnErr("Failed to create uint256[2] type", err)
	}
	uint256ArrayArray, err := abi.NewType("uint256[2][2]", "", nil)
	if err != nil {
		log.Fatal().AnErr("Failed to create uint256[2][2] type", err)
	}
	args := abi.Arguments{
		{Type: uint256Array},
		{Type: uint256ArrayArray},
		{Type: uint256Array},
	}
	encodedProofBytes, err := args.Pack(output.A, output.B, output.C)
	if err != nil {
		log.Fatal().AnErr("Failed to encode proof", err)
	}

	log.Info().Msg("Saving proof to proof.json")
	jsonProof, err := json.Marshal(types.ProofResult{
		// Output will be filled in by plonky2x CLI
		Output: []byte{},
		Proof:  encodedProofBytes,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal proof: %w", err)
	}
	proofFile, err := os.Create("proof.json")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create proof file: %w", err)
	}
	_, err = proofFile.Write(jsonProof)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to write proof file: %w", err)
	}
	proofFile.Close()
	log.Info().Msg("Successfully saved proof")

	publicWitness, err := witness.Public()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get public witness: %w", err)
	}

	vecWitness := publicWitness.Vector()
	fmt.Printf("Public witness: %v\n", vecWitness)
	fmt.Printf("%v %v %v\n", assignment.VerifierDigest, assignment.InputHash, assignment.OutputHash)

	log.Info().Msg("Saving public witness to public_witness.bin")
	witnessFile, err := os.Create("public_witness.bin")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create public witness file: %w", err)
	}
	_, err = publicWitness.WriteTo(witnessFile)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to write public witness file: %w", err)
	}
	witnessFile.Close()
	log.Info().Msg("Successfully saved public witness")

	return proof, publicWitness, nil
}
