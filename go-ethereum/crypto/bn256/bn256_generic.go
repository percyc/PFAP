// Copyright 2018 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
//go:build generic
// +build generic

package bn256

import "github.com/ethereum/go-ethereum/crypto/bn256/google"

// Generic aliases avoid assembly/toolchain compatibility issues in this
// historic geth fork. They trade some pairing performance for portability.
type G1 = bn256.G1
type G2 = bn256.G2

func PairingCheck(a []*G1, b []*G2) bool {
	return bn256.PairingCheck(a, b)
}
